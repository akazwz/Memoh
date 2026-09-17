package grok

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/felinics/memoh/internal/agent/runtime/agentstate"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/runtimefence"
	"github.com/felinics/memoh/internal/workspace/bridge"
	pb "github.com/felinics/memoh/internal/workspace/bridgepb"
)

const (
	snapshotFile       = "grok-snapshot-v1.jsonl"
	snapshotChunkBytes = 192 * 1024
	snapshotMaxBytes   = 256 * 1024 * 1024
	snapshotMaxEntries = 16384
)

type snapshotFS interface {
	ListDirBounded(context.Context, string, bool, int32) ([]*pb.FileEntry, error)
	ReadRawNoFollow(context.Context, string, string) (io.ReadCloser, error)
	WriteRawNoFollow(context.Context, string, string, io.Reader) (int64, error)
}

// Only native session trees are captured. Authentication, global settings,
// sockets, caches and MCP configuration outside these trees are never restored.
func safeSnapshotPath(p string) bool {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || strings.ContainsAny(p, "\\\x00") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "." || part == "" {
			return false
		}
	}
	return strings.HasPrefix(p, "sessions/") || strings.HasPrefix(p, "workspace/sessions/")
}

type snapshotChunk struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Data   []byte `json:"data"`
	Last   bool   `json:"last"`
}

// A temporary spool bounds memory to one chunk while preserving native bytes
// (including mutable JSON and binary artifacts) as immutable JSONL records.
type snapshotSpool struct {
	*os.File
	records int64
	digest  string
}

func (s *snapshotSpool) close() {
	name := s.Name()
	_ = s.Close()
	_ = os.Remove(name) // #nosec G703 -- path comes only from os.CreateTemp above.
}

func captureSnapshot(ctx context.Context, fs snapshotFS, home string) (out *snapshotSpool, err error) {
	file, err := os.CreateTemp("", "memoh-grok-snapshot-*")
	if err != nil {
		return nil, err
	}
	out = &snapshotSpool{File: file}
	defer func() {
		if err != nil {
			out.close()
		}
	}()
	hash := sha256.New()
	writer := io.MultiWriter(file, hash)
	encoder := json.NewEncoder(writer)
	var total int64
	for _, tree := range []string{"sessions", "workspace/sessions"} {
		entries, e := fs.ListDirBounded(ctx, path.Join(home, tree), true, snapshotMaxEntries)
		if errors.Is(e, bridge.ErrNotFound) {
			continue
		}
		if e != nil {
			return out, e
		}
		slices.SortFunc(entries, func(a, b *pb.FileEntry) int { return strings.Compare(a.GetPath(), b.GetPath()) })
		for _, entry := range entries {
			if entry.GetIsDir() {
				continue
			}
			// Refuse symlinks and special files instead of following them outside the
			// process-owned tree. ReadRawNoFollow also checks every path component.
			if strings.HasPrefix(entry.GetMode(), "L") || strings.HasPrefix(entry.GetMode(), "p") || strings.HasPrefix(entry.GetMode(), "S") {
				return out, errors.New("unsafe native session file")
			}
			entryPath := entry.GetPath()
			if entryPath == "" || path.IsAbs(entryPath) || path.Clean(entryPath) != entryPath || strings.HasPrefix(entryPath, "../") {
				return out, errors.New("unsafe native session path")
			}
			rel := path.Join(tree, entryPath)
			if !safeSnapshotPath(rel) {
				return out, errors.New("unsafe native session path")
			}
			r, e := fs.ReadRawNoFollow(ctx, home, rel)
			if e != nil {
				return out, e
			}
			var offset int64
			for {
				buf := make([]byte, snapshotChunkBytes)
				n, readErr := io.ReadFull(r, buf)
				if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
					_ = r.Close()
					return out, readErr
				}
				total += int64(n)
				if total > snapshotMaxBytes {
					_ = r.Close()
					return out, errors.New("grok snapshot exceeds size limit")
				}
				last := readErr != nil
				if e = encoder.Encode(snapshotChunk{Path: rel, Offset: offset, Data: buf[:n], Last: last}); e != nil {
					_ = r.Close()
					return out, e
				}
				out.records++
				offset += int64(n)
				if last {
					break
				}
			}
			if e = r.Close(); e != nil {
				return out, e
			}
		}
	}
	if out.records == 0 {
		return out, errors.New("grok did not persist a session")
	}
	out.digest = hex.EncodeToString(hash.Sum(nil))
	_, err = file.Seek(0, io.SeekStart)
	return out, err
}

func (s *snapshotSpool) reader() agentstate.SessionStateRecordReader {
	scanner := bufio.NewScanner(s.File)
	scanner.Buffer(make([]byte, 4096), snapshotChunkBytes*2)
	var line int64
	return func(ctx context.Context) (agentstate.SessionStateRecord, error) {
		if err := ctx.Err(); err != nil {
			return agentstate.SessionStateRecord{}, err
		}
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return agentstate.SessionStateRecord{}, err
			}
			return agentstate.SessionStateRecord{}, io.EOF
		}
		line++
		return agentstate.SessionStateRecord{FilePath: snapshotFile, LineNumber: line, Content: append(json.RawMessage(nil), scanner.Bytes()...)}, nil
	}
}

func (d *Driver) stage(ctx context.Context, fs snapshotFS, input external.PromptInput, home, sessionID, cwd string, epoch agentstate.RuntimeConfigEpoch) error {
	fence, ok := runtimefence.FromContext(ctx)
	if !ok {
		return errors.New("grok checkpoint requires runtime fence")
	}
	actual, err := d.stateStore.RuntimeConfigEpoch(ctx, input.BotID, input.ThreadID)
	if err != nil {
		return err
	}
	if actual != epoch {
		return agentstate.ErrRuntimeConfigStale
	}
	spool, err := captureSnapshot(ctx, fs, home)
	if err != nil {
		return err
	}
	defer spool.close()
	return d.stateStore.Replace(ctx, input.BotID, input.ThreadID, agentstate.PersistedSessionState{
		AgentID: RuntimeType, AgentSessionID: sessionID, ThroughRunID: input.RunID, StorageRevision: input.RunID, Cwd: cwd, TranscriptPath: snapshotFile, RuntimeFencingToken: fence.Token,
		FileCount: 1, RecordCount: spool.records, Files: []agentstate.PersistedSessionStateFile{{SessionStateFileShape: agentstate.SessionStateFileShape{Path: snapshotFile, Records: spool.records, Digest: spool.digest}}},
	}, spool.reader())
}

func restoreSnapshot(ctx context.Context, fs snapshotFS, home string, next agentstate.SessionStateRecordReader) error {
	// Spool each file so bridge writes remain atomic and memory stays bounded.
	var file *os.File
	var current string
	var offset, total int64
	closed := true
	seen := map[string]bool{}
	cleanup := func() {
		if file != nil {
			name := file.Name()
			_ = file.Close()
			_ = os.Remove(name) // #nosec G703 -- path comes only from os.CreateTemp below.
			file = nil
		}
	}
	defer cleanup()
	for {
		record, err := next(ctx)
		if errors.Is(err, io.EOF) {
			if !closed {
				return errors.New("incomplete Grok snapshot file")
			}
			return nil
		}
		if err != nil {
			return err
		}
		var chunk snapshotChunk
		if record.FilePath != snapshotFile || json.Unmarshal(record.Content, &chunk) != nil || !safeSnapshotPath(chunk.Path) || len(chunk.Data) > snapshotChunkBytes {
			return errors.New("invalid Grok snapshot record")
		}
		if closed {
			if seen[chunk.Path] || chunk.Offset != 0 {
				return errors.New("duplicate Grok snapshot file")
			}
			seen[chunk.Path] = true
			file, err = os.CreateTemp("", "memoh-grok-restore-*")
			if err != nil {
				return err
			}
			current = chunk.Path
			offset = 0
			closed = false
		}
		if chunk.Path != current || chunk.Offset != offset {
			return errors.New("unordered Grok snapshot chunks")
		}
		total += int64(len(chunk.Data))
		if total > snapshotMaxBytes || len(seen) > snapshotMaxEntries {
			return errors.New("grok snapshot exceeds size limit")
		}
		if _, err = file.Write(chunk.Data); err != nil {
			return err
		}
		offset += int64(len(chunk.Data))
		if chunk.Last {
			if _, err = file.Seek(0, io.SeekStart); err != nil {
				return err
			}
			if _, err = fs.WriteRawNoFollow(ctx, home, current, file); err != nil {
				return fmt.Errorf("restore Grok session file: %w", err)
			}
			cleanup()
			closed = true
		}
	}
}

func (d *Driver) restore(ctx context.Context, fs snapshotFS, input external.PromptInput, home, cwd string) (string, error) {
	if input.ForceFreshRuntime {
		return "", nil
	}
	var sessionID string
	_, err := d.stateStore.Load(ctx, input.BotID, input.ThreadID, func(ctx context.Context, state agentstate.PersistedSessionState, next agentstate.SessionStateRecordReader) error {
		if state.AgentID != RuntimeType || state.StorageRevision == "" || state.TranscriptPath != snapshotFile || state.Cwd != cwd {
			return errors.New("incompatible Grok checkpoint")
		}
		if !safeSessionID(state.AgentSessionID) {
			return errors.New("invalid Grok session id")
		}
		if err := restoreSnapshot(ctx, fs, home, next); err != nil {
			return err
		}
		sessionID = state.AgentSessionID
		return nil
	})
	return sessionID, err
}

func safeSessionID(id string) bool {
	return id != "" && len(id) < 256 && path.Base(id) == id && id != "." && id != ".." && !strings.ContainsAny(id, "\\\x00\r\n")
}
