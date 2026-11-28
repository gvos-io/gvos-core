// Recording play API (recording-oss-upgrade-plan R3): resolves a
// call's tracks from the recordings table and streams the artifact
// (LocalFS) or redirects to a presigned GET URL (S3/MinIO/OSS).
package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
