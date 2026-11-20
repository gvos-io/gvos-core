// Command gvos-asr bridges audio into real DashScope ASR.
//
// Modes:
//   gvos-asr file   -in speech.wav            transcribe a local file
//   gvos-asr bridge -api http://host:8080 -call <call-id>   subscribe a
//                    gv-core media stream, transcribe uplink audio live
//   gvos-asr feed   -in speech.wav -api http://host:8080 -call <id>  push
//                    file audio into a call's stream (test hook)
package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gvos-io/gvos-core/internal/mediamain/dashscope"
	"github.com/gvos-io/gvos-core/internal/mediamain/mediastream"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "file":
		err = cmdFile(os.Args[2:])
	case "bridge":
		err = cmdBridge(os.Args[2:])
	case "feed":
		err = cmdFeed(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  gvos-asr file   -in speech.wav [-model paraformer-realtime-v2]
  gvos-asr bridge -api http://127.0.0.1:8080 -call <call-id> [-token dev-admin-token]
  gvos-asr feed   -in speech.wav -api http://127.0.0.1:8080 -call <call-id> [-token dev-admin-token]
`)
}

func apiKey() string {
	k := os.Getenv("DASHSCOPE_API_KEY")
	if k == "" {
		k = os.Getenv("GVOS_ASR_KEY")
	}
	if k == "" {
		fmt.Fprintln(os.Stderr, "set DASHSCOPE_API_KEY")
		os.Exit(1)
	}
	return k
}

// cmdFile streams a local PCM/WAV file into one ASR session.
func cmdFile(args []string) error {
	fs := flag.NewFlagSet("file", flag.ExitOnError)
	in := fs.String("in", "", "input audio file (pcm/wav 16k)")
	model := fs.String("model", "paraformer-realtime-v2", "ASR model")
	fs.Parse(args)
	if *in == "" {
		return fmt.Errorf("-in required")
	}

	data, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	pcm, format, err := stripWav(data)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "audio: %d bytes pcm (format=%s)\n", len(pcm), format)

	sess, err := dashscope.NewSession(dashscope.Config{APIKey: apiKey(), Model: *model, Format: format})
	if err != nil {
		return err
	}
	defer sess.Close()

	go func() {
		for s := range sess.Results() {
			mark := "…"
			if s.Final {
				mark = "."
			}
			fmt.Printf("%s%s\n", s.Text, mark)
		}
	}()

	// 100ms frames paced (protocol expects ~realtime pacing).
	frame := pcmSampleBytesPer100ms()
	for off := 0; off < len(pcm); off += frame {
		end := off + frame
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := sess.PushAudio(pcm[off:end]); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	sess.Finish()

	deadline := time.After(15 * time.Second)
	for {
		select {
		case err := <-sess.Err():
			if err != nil {
				return err
			}
			return nil
		case <-deadline:
			return fmt.Errorf("timeout waiting for task-finished")
		}
	}
}

// cmdBridge subscribes to a gv-core media stream and transcribes its
// uplink audio live.
func cmdBridge(args []string) error {
	fs := flag.NewFlagSet("bridge", flag.ExitOnError)
	api := fs.String("api", "http://127.0.0.1:8080", "gv-core base URL")
	call := fs.String("call", "", "call id")
	fs.Parse(args)
	if *call == "" {
		return fmt.Errorf("-call required")
	}

	wsURL := strings.Replace(*api, "http://", "ws://", 1) + "/v1/stream/" + *call
	conn, err := mediastream.DialWS(wsURL, nil, 10*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	fmt.Fprintf(os.Stderr, "bridging %s\n", wsURL)

	sess, err := dashscope.NewSession(dashscope.Config{APIKey: apiKey()})
	if err != nil {
		return err
	}
	defer sess.Close()

	done := make(chan struct{})
	go func() {
		for s := range sess.Results() {
			mark := "…"
			if s.Final {
				mark = "."
			}
			fmt.Printf("[asr]%s %s%s\n", time.Now().Format("15:04:05"), s.Text, mark)
		}
		close(done)
	}()

	go func() {
		for {
			f, err := conn.Read()
			if err != nil {
				return
			}
			if f.Opcode != mediastream.OpBinaryValue || len(f.Payload) < 1 {
				continue
			}
			if f.Payload[0] == 0 { // uplink = caller audio
				_ = sess.PushAudio(f.Payload[1:])
			}
		}
	}()

	<-done
	return nil
}

// cmdFeed pushes a file's audio into a call's stream (drives live
// bridges/tests without a real caller).
func cmdFeed(args []string) error {
	fs := flag.NewFlagSet("feed", flag.ExitOnError)
	in := fs.String("in", "", "audio file (pcm/wav 16k)")
	api := fs.String("api", "http://127.0.0.1:8080", "gv-core base URL")
	call := fs.String("call", "", "call id")
	token := fs.String("token", "dev-admin-token", "admin token")
	pace := fs.Bool("pace", true, "pace at realtime speed")
	fs.Parse(args)
	if *in == "" || *call == "" {
		return fmt.Errorf("-in and -call required")
	}

	data, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	pcm, _, err := stripWav(data)
	if err != nil {
		return err
	}

	// The feed endpoint pushes telephony-side audio into the stream.
	frame := pcmSampleBytesPer100ms()
	url := *api + "/v1/admin/stream-push"
	for off := 0; off < len(pcm); off += frame {
		end := off + frame
		if end > len(pcm) {
			end = len(pcm)
		}
		body := map[string]any{"call_id": *call, "direction": "uplink", "audio": base64.StdEncoding.EncodeToString(pcm[off:end])}
		blob, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", url, strings.NewReader(string(blob)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+*token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("push rejected: %d", resp.StatusCode)
		}
		if *pace {
			time.Sleep(100 * time.Millisecond)
		}
	}
	fmt.Fprintf(os.Stderr, "fed %d bytes\n", len(pcm))
	return nil
}

// stripWav returns raw PCM and the ASR format hint. WAV headers are
// parsed minimally; raw data is passed through as pcm.
func stripWav(data []byte) ([]byte, string, error) {
	if len(data) < 44 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return data, "pcm", nil // assume raw PCM
	}
	// Walk chunks to find "data".
	off := 12
	for off+8 <= len(data) {
		id := string(data[off : off+4])
		size := int(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		if id == "data" {
			end := off + 8 + size
			if end > len(data) {
				end = len(data)
			}
			return data[off+8 : end], "wav", nil
		}
		off += 8 + size + (size & 1)
	}
	return nil, "", fmt.Errorf("wav: no data chunk")
}

func pcmSampleBytesPer100ms() int { return 3200 } // 16k*2bytes*0.1s
