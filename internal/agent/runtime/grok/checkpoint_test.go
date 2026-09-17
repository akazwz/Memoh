package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"reflect"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/runtime/agentstate"
	"github.com/felinics/memoh/internal/workspace/bridge"
	pb "github.com/felinics/memoh/internal/workspace/bridgepb"
)

type memorySnapshotFS struct {
	files   map[string][]byte
	entries []*pb.FileEntry
}

func (f *memorySnapshotFS) ListDirBounded(_ context.Context, root string, _ bool, _ int32) ([]*pb.FileEntry, error) {
	if f.entries != nil {
		return f.entries, nil
	}
	entries := []*pb.FileEntry{}
	for name := range f.files {
		if strings.HasPrefix(name, root+"/") {
			entries = append(entries, &pb.FileEntry{Path: strings.TrimPrefix(name, root+"/"), Mode: "-rw-------"})
		}
	}
	if len(entries) == 0 {
		return nil, bridge.ErrNotFound
	}
	return entries, nil
}

func (f *memorySnapshotFS) ReadRawNoFollow(_ context.Context, root, rel string) (io.ReadCloser, error) {
	data, ok := f.files[path.Join(root, rel)]
	if !ok {
		return nil, bridge.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *memorySnapshotFS) WriteRawNoFollow(_ context.Context, root, rel string, r io.Reader) (int64, error) {
	name := path.Join(root, rel)
	if _, ok := f.files[name]; ok {
		return 0, errors.New("already exists")
	}
	data, err := io.ReadAll(r)
	if err == nil {
		f.files[name] = data
	}
	return int64(len(data)), err
}

func TestSnapshotRoundTripPreservesMutableNativeBytesAndExcludesCredentials(t *testing.T) {
	ctx := context.Background()
	fs := &memorySnapshotFS{files: map[string][]byte{
		"/lease/sessions/data/s1/events.jsonl":         []byte("{\"x\": 1}\n\n"),
		"/lease/sessions/data/s1/summary.json":         []byte("{\"model\":\"grok\"}"),
		"/lease/workspace/sessions/s1/tool_state/blob": bytes.Repeat([]byte{0, 255, 1}, snapshotChunkBytes),
		"/lease/auth.json":                             []byte("never capture"),
		"/lease/config.toml":                           []byte("never capture"),
	}}
	first, err := captureSnapshot(ctx, fs, "/lease")
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	fs.files["/lease/sessions/data/s1/summary.json"] = []byte("{\"model\":\"changed\"}")
	second, err := captureSnapshot(ctx, fs, "/lease")
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	if first.digest == second.digest {
		t.Fatal("mutable state did not change revision digest")
	}
	target := &memorySnapshotFS{files: map[string][]byte{}}
	if err = restoreSnapshot(ctx, target, "/target", first.reader()); err != nil {
		t.Fatal(err)
	}
	if got := string(target.files["/target/sessions/data/s1/summary.json"]); got != "{\"model\":\"grok\"}" {
		t.Fatalf("old snapshot changed: %s", got)
	}
	if len(target.files) != 3 {
		t.Fatalf("unexpected captured files: %v", target.files)
	}
	if !bytes.Equal(target.files["/target/workspace/sessions/s1/tool_state/blob"], fs.files["/lease/workspace/sessions/s1/tool_state/blob"]) {
		t.Fatal("binary chunks changed")
	}
}

func TestSnapshotRejectsTraversalAndSymlinksBeforeReading(t *testing.T) {
	for _, entry := range []*pb.FileEntry{{Path: "../sessions/escape", Mode: "-rw-------"}, {Path: "/absolute", Mode: "-rw-------"}, {Path: "safe", Mode: "Lrwxrwxrwx"}} {
		fs := &memorySnapshotFS{entries: []*pb.FileEntry{entry}}
		if spool, err := captureSnapshot(context.Background(), fs, "/lease"); err == nil {
			spool.close()
			t.Fatalf("accepted unsafe entry %v", entry)
		}
	}
}

func chunkReader(chunks ...snapshotChunk) agentstate.SessionStateRecordReader {
	index := 0
	return func(context.Context) (agentstate.SessionStateRecord, error) {
		if index == len(chunks) {
			return agentstate.SessionStateRecord{}, io.EOF
		}
		raw, _ := json.Marshal(chunks[index])
		index++
		return agentstate.SessionStateRecord{FilePath: snapshotFile, LineNumber: int64(index), Content: raw}, nil
	}
}

func TestRestoreRejectsIncompleteOverlappingAndEscapingChunks(t *testing.T) {
	for _, chunks := range [][]snapshotChunk{
		{{Path: "sessions/s1/data", Data: []byte("partial")}},
		{{Path: "sessions/../../auth.json", Last: true}},
		{{Path: "sessions/s1/data", Offset: 1, Last: true}},
		{{Path: "sessions/s1/data", Data: []byte("a")}, {Path: "sessions/s1/data", Offset: 0, Last: true}},
		{{Path: "sessions/s1/data", Last: true}, {Path: "sessions/s1/data", Last: true}},
	} {
		fs := &memorySnapshotFS{files: map[string][]byte{}}
		if err := restoreSnapshot(context.Background(), fs, "/lease", chunkReader(chunks...)); err == nil {
			t.Fatalf("accepted corrupt chunks: %v", chunks)
		}
	}
}

func TestPromptBoundaryKeepsNativeIdentityAndExclusiveCut(t *testing.T) {
	want := promptBoundary{SessionID: "s1", PromptID: "prompt-7", NextPromptIndex: 8}
	value := encodeBoundary(want.SessionID, want.PromptID, want.NextPromptIndex)
	got, err := decodeBoundary(value)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("boundary = %#v, %v", got, err)
	}
	for _, bad := range []string{"", "prompt-7", "grok:bad", encodeBoundary("s1", "", 1)} {
		if _, err = decodeBoundary(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
