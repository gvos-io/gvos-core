// Recording play API (recording-oss-upgrade-plan R3): resolves a
// call's tracks from the recordings table and streams the artifact
// (LocalFS) or redirects to a presigned GET URL (S3/MinIO/OSS).
package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gvos-io/gvos-core/internal/recpipe"
)

// handleRecordingList serves GET /v1/recording/{call_id}: the track
// list (play URLs per track; S3 backends also expose presigned_url).
func (s *Server) handleRecordingList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	callID := r.PathValue("call_id")
	if s.pg == nil {
		httpErrorC(w, 503, "UNAVAILABLE", "recordings need postgres mode")
		return
	}
	rows, err := s.pg.GetRecordings(r.Context(), callID)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	backend := "local"
	if s.recStore != nil {
		backend = s.recStore.Kind()
	}
	out := []map[string]any{}
	for _, rr := range rows {
		item := map[string]any{
			"track": rr.Track, "format": rr.Format,
			"size_bytes": rr.SizeBytes, "backend": backend,
			"play_url": fmt.Sprintf("/v1/recording/%s/%s", callID, rr.Track),
		}
		if u := s.presignRecording(r, rr.Locator); u != "" {
			item["presigned_url"] = u
		}
		out = append(out, item)
	}
	writeJSON(w, 200, map[string]any{"call_id": callID, "tracks": out, "count": len(out)})
}

// handleRecordingPlay serves GET /v1/recording/{call_id}/{track}:
// LocalFS streams MP3; S3 backends 302 to a presigned URL.
func (s *Server) handleRecordingPlay(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	callID := r.PathValue("call_id")
	track := r.PathValue("track")
	if s.pg == nil {
		httpErrorC(w, 503, "UNAVAILABLE", "recordings need postgres mode")
		return
	}
	rows, err := s.pg.GetRecordings(r.Context(), callID)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	for _, rr := range rows {
		if rr.Track != track {
			continue
		}
		if u := s.presignRecording(r, rr.Locator); u != "" {
			http.Redirect(w, r, u, http.StatusFound)
			return
		}
		// Defense in depth: the locator rides in the DB, so only open
		// file paths that sit inside the configured archive root (the
		// write path is controlled, but nothing else vouches for the
		// column).
		if lfs, ok := s.recStore.(*recpipe.LocalFS); ok {
			root := filepath.Clean(lfs.Root) + string(os.PathSeparator)
			if !strings.HasPrefix(filepath.Clean(rr.Locator)+string(os.PathSeparator), root) {
				httpErrorC(w, 404, "NOT_FOUND", "artifact outside archive root")
				return
			}
		} else if s.recStore != nil {
			// S3 backend: only s3:// locators are served (presign path
			// above); a file-path locator has no trusted root to check.
			httpErrorC(w, 404, "NOT_FOUND", "artifact missing from backend")
			return
		}
		f, err := os.Open(rr.Locator) // locator = LocalFS absolute path
		if err != nil {
			httpErrorC(w, 404, "NOT_FOUND", "artifact missing from backend: "+err.Error())
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Disposition", `inline; filename="`+callID+`-`+track+`.mp3"`)
		st, _ := f.Stat()
		http.ServeContent(w, r, "", st.ModTime(), f)
		return
	}
	httpErrorC(w, 404, "NOT_FOUND", "no recording for track "+track)
}

func (s *Server) presignRecording(r *http.Request, locator string) string {
	if s.recStore == nil || !strings.HasPrefix(locator, "s3://") {
		return ""
	}
	ps, ok := s.recStore.(recpipe.Presigner)
	if !ok {
		return ""
	}
	m, ok := s.recStore.(*recpipe.MinIO)
	if !ok {
		return ""
	}
	key, ok := m.ObjectKeyFromLocator(locator)
	if !ok {
		return ""
	}
	u, err := ps.PresignGet(r.Context(), key, 5*time.Minute)
	if err != nil {
		return ""
	}
	return u
}

// DELETE /v1/recording/{call_id}/{track}: removes the track row AND
// its storage object (S3 backend: Presigner/Deleter face when
// implemented; LocalFS: resolved against the archive root).
func (s *Server) handleRecordingDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.pg == nil {
		httpErrorC(w, 503, "UNAVAILABLE", "recording administration requires PG mode")
		return
	}
	callID, track := r.PathValue("call_id"), r.PathValue("track")
	objectKey, locator, err := s.pg.GetRecording(r.Context(), callID, track)
	if err != nil {
		httpErrorC(w, 404, "NOT_FOUND", "no recording for "+callID+"/"+track)
		return
	}
	// Storage delete is best-effort: a lost file must not pin the row.
	if s.recStore != nil {
		if d, ok := s.recStore.(recpipe.Deleter); ok {
			key := objectKey
			if strings.HasPrefix(locator, "s3://") {
				// s3://bucket/key → key
				if i := strings.Index(locator[5:], "/"); i >= 0 {
					key = locator[5+i+1:]
				}
			}
			if derr := d.Delete(r.Context(), key); derr != nil {
				log.Printf("recording storage delete failed (%s/%s): %v", callID, track, derr)
			}
		}
	}
	if n, err := s.pg.DeleteRecordingRow(r.Context(), callID, track); err != nil {
		httpError(w, 500, err.Error())
		return
	} else if n == 0 {
		httpErrorC(w, 404, "NOT_FOUND", "no recording for "+callID+"/"+track)
		return
	}
	s.audit(r, "delete", "recording", callID+"/"+track, nil, nil)
	writeJSON(w, 200, map[string]any{"deleted": callID + "/" + track})
}

// recordingRetentionDays: opt-in retention (0 = keep forever; the
// purge pass only runs when set). GVOS_RECORDING_TTL_DAYS.
func recordingRetentionDays() int {
	if v := os.Getenv("GVOS_RECORDING_TTL_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// startRecordingRetention runs the hourly retention purge when a TTL
// is configured: storage objects first, then the rows.
func (s *Server) startRecordingRetention() {
	days := recordingRetentionDays()
	if days == 0 || s.pg == nil {
		return
	}
	go func() {
		run := func() {
			cctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			rows, err := s.pg.ListExpiredRecordings(cctx, days)
			if err != nil {
				log.Printf("recording retention list failed: %v", err)
				return
			}
			removed := 0
			for _, r := range rows {
				if s.recStore != nil {
					if d, ok := s.recStore.(recpipe.Deleter); ok {
						_ = d.Delete(cctx, r.ObjectKey)
					}
				}
				removed++
			}
			if n, err := s.pg.PurgeRecordingRows(cctx, days); err != nil {
				log.Printf("recording retention purge failed: %v", err)
			} else if n > 0 {
				log.Printf("recording retention: purged %d rows (%d objects attempted)", n, removed)
			}
		}
		run()
		tk := time.NewTicker(time.Hour)
		defer tk.Stop()
		for range tk.C {
			run()
		}
	}()
	log.Printf("recording retention on: %d days", days)
}
