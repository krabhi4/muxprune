package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/krabhi4/muxprune/internal/engine"
	"github.com/krabhi4/muxprune/internal/jobs"
	"github.com/krabhi4/muxprune/internal/probe"
	"github.com/krabhi4/muxprune/internal/store"
)

type jsonRPCRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Result  any              `json:"result,omitempty"`
	Error   *jsonRPCError    `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ServeMCP launches the Model Context Protocol (MCP) server over standard input/output.
// It redirects normal logging prints on stdout to stderr to avoid corrupting the JSON-RPC channel.
func (s *Server) ServeMCP(ctx context.Context) error {
	// Save the original stdout for protocol messages.
	originalStdout := os.Stdout
	// Redirect any standard fmt.Printf or log output to stderr to keep stdout clean.
	os.Stdout = os.Stderr

	reader := bufio.NewReader(os.Stdin)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line, err := reader.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			sendMCPError(originalStdout, nil, -32700, "Parse error: "+err.Error())
			continue
		}

		if req.Method == "" {
			continue // ignore notifications without methods or invalid json
		}

		s.handleMCPRequest(ctx, originalStdout, &req)
	}
}

func (s *Server) handleMCPRequest(ctx context.Context, w io.Writer, req *jsonRPCRequest) {
	switch req.Method {
	case "initialize":
		sendMCPResponse(w, req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "muxprune-mcp",
				"version": "v0.2.0",
			},
		})

	case "notifications/initialized", "initialized":
		// No response required for notifications

	case "tools/list":
		tools := []map[string]any{
			{
				"name":        "list_libraries",
				"description": "List all configured media libraries.",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
			{
				"name":        "list_files",
				"description": "List or search media files in libraries.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"library_id": map[string]any{"type": "integer", "description": "Filter by library ID"},
						"q":          map[string]any{"type": "string", "description": "Search query for title or path"},
						"limit":      map[string]any{"type": "integer", "description": "Maximum results (default 50)"},
					},
				},
			},
			{
				"name":        "get_file",
				"description": "Get detailed stream layout and sidecars of a media file.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id": map[string]any{"type": "integer", "description": "The file ID"},
					},
					"required": []string{"id"},
				},
			},
			{
				"name":        "queue_strip_job",
				"description": "Queue a job to strip unwanted audio/subtitle tracks from a file.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id":        map[string]any{"type": "integer", "description": "The file ID"},
						"audio_idx":      map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Stream indexes of audio tracks to remove"},
						"sub_idx":        map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Stream indexes of subtitle tracks to remove"},
						"allow_hardlink": map[string]any{"type": "boolean", "description": "Allow remuxing even if file is hardlinked (breaks seed links)"},
					},
					"required": []string{"file_id"},
				},
			},
			{
				"name":        "queue_metadata_job",
				"description": "Queue a job to edit metadata headers in-place (language, title, default/forced flags).",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{"type": "integer", "description": "The file ID"},
						"edits": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"track_index": map[string]any{"type": "integer", "description": "The ffprobe stream index"},
									"language":    map[string]any{"type": "string", "description": "ISO 639-2 language tag (e.g. eng, jpn)"},
									"title":       map[string]any{"type": "string", "description": "Track title/name"},
									"default":     map[string]any{"type": "boolean", "description": "Set flag-default (true/false)"},
									"forced":      map[string]any{"type": "boolean", "description": "Set flag-forced (true/false)"},
								},
								"required": []string{"track_index"},
							},
							"description": "Array of metadata edits to apply",
						},
					},
					"required": []string{"file_id", "edits"},
				},
			},
			{
				"name":        "queue_reorder_job",
				"description": "Queue a job to reorder tracks in a Matroska container.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id":     map[string]any{"type": "integer", "description": "The file ID"},
						"track_order": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Desired track order of ffprobe stream indexes"},
					},
					"required": []string{"file_id", "track_order"},
				},
			},
			{
				"name":        "queue_merge_job",
				"description": "Queue a job to merge external subtitle/audio files into a Matroska container.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id":        map[string]any{"type": "integer", "description": "The file ID"},
						"external_files": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Absolute paths on server of external files to merge"},
					},
					"required": []string{"file_id", "external_files"},
				},
			},
			{
				"name":        "get_job",
				"description": "Get status, log, and saved space of a specific job.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id": map[string]any{"type": "integer", "description": "The job ID"},
					},
					"required": []string{"id"},
				},
			},
		}
		sendMCPResponse(w, req.ID, map[string]any{"tools": tools})

	case "tools/call":
		var callParams struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &callParams); err != nil {
			sendMCPError(w, req.ID, -32602, "Invalid params: "+err.Error())
			return
		}

		s.handleMCPToolCall(ctx, w, req.ID, callParams.Name, callParams.Arguments)

	default:
		sendMCPError(w, req.ID, -32601, "Method not found: "+req.Method)
	}
}

func (s *Server) handleMCPToolCall(ctx context.Context, w io.Writer, id *json.RawMessage, toolName string, args json.RawMessage) {
	switch toolName {
	case "list_libraries":
		libs, err := s.Store.ListLibraries()
		if err != nil {
			sendMCPToolError(w, id, err.Error())
			return
		}
		b, _ := json.MarshalIndent(libs, "", "  ")
		sendMCPToolResult(w, id, string(b))

	case "list_files":
		var filter struct {
			LibraryID int64  `json:"library_id"`
			Q         string `json:"q"`
			Limit     int    `json:"limit"`
		}
		_ = json.Unmarshal(args, &filter)
		if filter.Limit <= 0 {
			filter.Limit = 50
		}
		files, _, err := s.Store.ListFiles(store.FileFilter{
			LibraryID: filter.LibraryID,
			Query:     filter.Q,
			Sort:      "default",
			Order:     "asc",
			Limit:     filter.Limit,
		})
		if err != nil {
			sendMCPToolError(w, id, err.Error())
			return
		}
		b, _ := json.MarshalIndent(files, "", "  ")
		sendMCPToolResult(w, id, string(b))

	case "get_file":
		var fArgs struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(args, &fArgs); err != nil || fArgs.ID == 0 {
			sendMCPToolError(w, id, "Invalid or missing file 'id' argument")
			return
		}
		detail, _, err := s.loadDetail(fArgs.ID)
		if err != nil {
			sendMCPToolError(w, id, err.Error())
			return
		}
		if detail == nil {
			sendMCPToolError(w, id, "File not found")
			return
		}
		b, _ := json.MarshalIndent(detail, "", "  ")
		sendMCPToolResult(w, id, string(b))

	case "queue_strip_job":
		var sArgs struct {
			FileID        int64 `json:"file_id"`
			AudioIdx      []int `json:"audio_idx"`
			SubIdx        []int `json:"sub_idx"`
			AllowHardlink bool  `json:"allow_hardlink"`
		}
		if err := json.Unmarshal(args, &sArgs); err != nil || sArgs.FileID == 0 {
			sendMCPToolError(w, id, "Invalid or missing 'file_id' argument")
			return
		}
		detail, _, err := s.loadDetail(sArgs.FileID)
		if err != nil {
			sendMCPToolError(w, id, err.Error())
			return
		}
		if detail == nil {
			sendMCPToolError(w, id, "File not found")
			return
		}

		var queued []*store.Job
		if len(sArgs.AudioIdx) > 0 || len(sArgs.SubIdx) > 0 {
			j, err := s.Store.CreateJob("remux", detail.ID, detail.Path, jobs.RemuxPayload{
				AudioIdx:       sArgs.AudioIdx,
				SubIdx:         sArgs.SubIdx,
				AllowHardlink:  sArgs.AllowHardlink,
				AllowLastAudio: false,
			})
			if err != nil {
				sendMCPToolError(w, id, err.Error())
				return
			}
			queued = append(queued, j)
		}
		s.Runner.Wake()
		b, _ := json.MarshalIndent(queued, "", "  ")
		sendMCPToolResult(w, id, "Strip job queued:\n"+string(b))

	case "queue_metadata_job":
		var mArgs struct {
			FileID int64                 `json:"file_id"`
			Edits  []engine.MetadataEdit `json:"edits"`
		}
		if err := json.Unmarshal(args, &mArgs); err != nil || mArgs.FileID == 0 || len(mArgs.Edits) == 0 {
			sendMCPToolError(w, id, "Invalid arguments; edits list and file_id required")
			return
		}
		detail, res, err := s.loadDetail(mArgs.FileID)
		if err != nil {
			sendMCPToolError(w, id, err.Error())
			return
		}
		if detail == nil {
			sendMCPToolError(w, id, "File not found")
			return
		}
		if res == nil || !res.IsMatroska() {
			sendMCPToolError(w, id, "Metadata editing requires a Matroska container")
			return
		}
		j, err := s.Store.CreateJob("edit_metadata", detail.ID, detail.Path, jobs.EditMetadataPayload{
			Edits: mArgs.Edits,
		})
		if err != nil {
			sendMCPToolError(w, id, err.Error())
			return
		}
		s.Runner.Wake()
		b, _ := json.MarshalIndent(j, "", "  ")
		sendMCPToolResult(w, id, "Metadata edit job queued:\n"+string(b))

	case "queue_reorder_job":
		var rArgs struct {
			FileID     int64 `json:"file_id"`
			TrackOrder []int `json:"track_order"`
		}
		if err := json.Unmarshal(args, &rArgs); err != nil || rArgs.FileID == 0 || len(rArgs.TrackOrder) == 0 {
			sendMCPToolError(w, id, "Invalid arguments; track_order list and file_id required")
			return
		}
		detail, res, err := s.loadDetail(rArgs.FileID)
		if err != nil {
			sendMCPToolError(w, id, err.Error())
			return
		}
		if detail == nil {
			sendMCPToolError(w, id, "File not found")
			return
		}
		if res == nil || !res.IsMatroska() {
			sendMCPToolError(w, id, "Track reordering requires a Matroska container")
			return
		}

		// Validate order matches expectations
		byIdx := map[int]probe.Stream{}
		for _, s := range res.Streams {
			byIdx[s.Index] = s
		}
		expectedCount := 0
		for _, s := range res.Streams {
			if s.MkvID >= 0 {
				expectedCount++
			}
		}
		if len(rArgs.TrackOrder) != expectedCount {
			sendMCPToolError(w, id, fmt.Sprintf("track order must contain exactly %d tracks, got %d", expectedCount, len(rArgs.TrackOrder)))
			return
		}
		seen := map[int]bool{}
		for _, idx := range rArgs.TrackOrder {
			if seen[idx] {
				sendMCPToolError(w, id, fmt.Sprintf("duplicate stream index %d in track order", idx))
				return
			}
			seen[idx] = true
			st, ok := byIdx[idx]
			if !ok {
				sendMCPToolError(w, id, fmt.Sprintf("stream index %d not found", idx))
				return
			}
			if st.MkvID < 0 {
				sendMCPToolError(w, id, fmt.Sprintf("stream index %d has no mkvmerge track ID", idx))
				return
			}
		}

		j, err := s.Store.CreateJob("reorder_tracks", detail.ID, detail.Path, jobs.ReorderPayload{
			TrackOrder: rArgs.TrackOrder,
		})
		if err != nil {
			sendMCPToolError(w, id, err.Error())
			return
		}
		s.Runner.Wake()
		b, _ := json.MarshalIndent(j, "", "  ")
		sendMCPToolResult(w, id, "Track reordering job queued:\n"+string(b))

	case "queue_merge_job":
		var mArgs struct {
			FileID        int64    `json:"file_id"`
			ExternalFiles []string `json:"external_files"`
		}
		if err := json.Unmarshal(args, &mArgs); err != nil || mArgs.FileID == 0 || len(mArgs.ExternalFiles) == 0 {
			sendMCPToolError(w, id, "Invalid arguments; external_files list and file_id required")
			return
		}
		detail, res, err := s.loadDetail(mArgs.FileID)
		if err != nil {
			sendMCPToolError(w, id, err.Error())
			return
		}
		if detail == nil {
			sendMCPToolError(w, id, "File not found")
			return
		}
		if res == nil || !res.IsMatroska() {
			sendMCPToolError(w, id, "Track merging requires a Matroska container")
			return
		}

		// Resolve absolute paths and validate
		absPath, err := filepath.Abs(detail.Path)
		if err != nil {
			sendMCPToolError(w, id, err.Error())
			return
		}
		seenExt := map[string]bool{}
		for _, ext := range mArgs.ExternalFiles {
			absExt, err := filepath.Abs(ext)
			if err != nil {
				sendMCPToolError(w, id, fmt.Sprintf("external file %s path: %v", ext, err))
				return
			}
			if absExt == absPath {
				sendMCPToolError(w, id, fmt.Sprintf("cannot merge a file into itself: %s", ext))
				return
			}
			if seenExt[absExt] {
				sendMCPToolError(w, id, fmt.Sprintf("duplicate external file: %s", ext))
				return
			}
			seenExt[absExt] = true

			fi, err := os.Stat(ext)
			if err != nil {
				sendMCPToolError(w, id, fmt.Sprintf("external file %s: %v", ext, err))
				return
			}
			if fi.IsDir() {
				sendMCPToolError(w, id, fmt.Sprintf("external file %s is a directory", ext))
				return
			}
		}

		j, err := s.Store.CreateJob("merge_tracks", detail.ID, detail.Path, jobs.MergePayload{
			ExternalFiles: mArgs.ExternalFiles,
		})
		if err != nil {
			sendMCPToolError(w, id, err.Error())
			return
		}
		s.Runner.Wake()
		b, _ := json.MarshalIndent(j, "", "  ")
		sendMCPToolResult(w, id, "Track merge job queued:\n"+string(b))

	case "get_job":
		var jArgs struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(args, &jArgs); err != nil || jArgs.ID == 0 {
			sendMCPToolError(w, id, "Invalid or missing job 'id' argument")
			return
		}
		job, err := s.Store.GetJob(jArgs.ID)
		if err != nil {
			sendMCPToolError(w, id, err.Error())
			return
		}
		if job == nil {
			sendMCPToolError(w, id, "Job not found")
			return
		}
		b, _ := json.MarshalIndent(job, "", "  ")
		sendMCPToolResult(w, id, string(b))

	default:
		sendMCPToolError(w, id, "Unknown tool: "+toolName)
	}
}

func sendMCPResponse(w io.Writer, id *json.RawMessage, result any) {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	b, _ := json.Marshal(resp)
	_, _ = w.Write(append(b, '\n'))
}

func sendMCPError(w io.Writer, id *json.RawMessage, code int, msg string) {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &jsonRPCError{
			Code:    code,
			Message: msg,
		},
	}
	b, _ := json.Marshal(resp)
	_, _ = w.Write(append(b, '\n'))
}

func sendMCPToolResult(w io.Writer, id *json.RawMessage, text string) {
	sendMCPResponse(w, id, map[string]any{
		"content": []map[string]any{
			{
				"type": "text",
				"text": text,
			},
		},
	})
}

func sendMCPToolError(w io.Writer, id *json.RawMessage, text string) {
	sendMCPResponse(w, id, map[string]any{
		"content": []map[string]any{
			{
				"type": "text",
				"text": text,
			},
		},
		"isError": true,
	})
}
