// Recording play API (recording-oss-upgrade-plan R3): resolves a
// call's tracks from the recordings table and streams the artifact.
// Local backend: the gv-core process streams from its archive dir
// (same host as the uploader's LocalFS root); OSS backend: the
// presigned URL replaces the stream (Phase R4 wiring).
package api

import (
	"fmt"
	"net/http"
	"os"
)

// handleRecordingList serves GET /v1/recording/{call_id}: the track
// list (play URLs per track).
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
	out := []map[string]any{}
	for _, rr := range rows {
		out = append(out, map[string]any{
			"track": rr.Track, "format": rr.Format,
			"size_bytes": rr.SizeBytes, "backend": "local",
			"play_url": fmt.Sprintf("/v1/recording/%s/%s", callID, rr.Track),
		})
	}
	writeJSON(w, 200, map[string]any{"call_id": callID, "tracks": out, "count": len(out)})
}

// handleRecordingPlay serves GET /v1/recording/{call_id}/{track}:
// streams the MP3 from the local archive (browser-ready playback).
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
		f, err := os.Open(rr.Locator) // locator = LocalFS absolute path
		if err != nil {
			httpErrorC(w, 404, "NOT_FOUND", "artifact missing from backend: "+err.Error())
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Disposition", `inline; filename="`+callID+`-`+track+`.mp3"`)
		if _, err := f.Seek(0, 0); err != nil {
			httpError(w, 500, err.Error())
			return
		}
		// Stream (http.ServeContent would add range support cheaply).
		st, _ := f.Stat()
		http.ServeContent(w, r, "", st.ModTime(), f)
		return
	}
	httpErrorC(w, 404, "NOT_FOUND", "no recording for track "+track)
}
