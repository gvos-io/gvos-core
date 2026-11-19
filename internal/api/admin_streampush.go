// Media stream test hook (internal; x-internal in OpenAPI).
package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/fastvoip/gvos-core/internal/mediamain/mediastream"
)

func (s *Server) handleStreamPush(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !requirePOST(w, r) {
		return
	}
	var req struct {
		CallID    string `json:"call_id"`
		Direction string `json:"direction"`
		AudioB64  string `json:"audio"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CallID == "" || req.AudioB64 == "" {
		httpError(w, 400, "call_id and audio (base64) required")
		return
	}
	audio, err := base64.StdEncoding.DecodeString(req.AudioB64)
	if err != nil {
		httpError(w, 400, "audio base64 invalid")
		return
	}
	stream := s.media.Get(req.CallID)
	if stream == nil {
		httpError(w, 404, "no live stream for call")
		return
	}
	dir := mediastream.Uplink
	if req.Direction == "downlink" {
		dir = mediastream.Downlink
	}
	c := stream.Push(dir, audio)
	writeJSON(w, 200, map[string]any{"call_id": req.CallID, "seq": c.Seq, "bytes": len(audio)})
}

// genSIPPassword produces a pronounceable-ish random SIP password.
