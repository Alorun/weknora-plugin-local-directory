package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/pkg/plugin/sdk"
)

const maxFiles = 1024
const maxEntries = 4096
const maxPathBytes = 512
const maxDepth = 32
const maxCursorBytes = sdk.MaxCursorBytes - 1024 // room for Host envelope serialization

type fileState struct {
	Digest   string `json:"digest"`
	Revision string `json:"revision"`
}
type snapshot struct {
	Version int                  `json:"version"`
	Round   string               `json:"round"` // string survives Host map[string]any without float precision loss
	Files   map[string]fileState `json:"files"`
}
type cursorEnvelope struct {
	LastSyncTime    json.RawMessage `json:"last_sync_time,omitempty"`
	LastSchemaHash  string          `json:"last_schema_hash,omitempty"`
	ConnectorCursor struct {
		LocalDirectory *snapshot `json:"local_directory"`
	} `json:"connector_cursor"`
}

func readCursor(data []byte) (snapshot, uint64, error) {
	if len(data) == 0 {
		return snapshot{Version: 1, Round: "0", Files: map[string]fileState{}}, 0, nil
	}
	var env cursorEnvelope
	if len(data) > sdk.MaxCursorBytes {
		return snapshot{}, 0, errors.New("cursor exceeds SDK limit")
	}
	if err := decodeStrict(data, &env); err != nil {
		return snapshot{}, 0, errors.New("invalid cursor JSON")
	}
	s := env.ConnectorCursor.LocalDirectory
	if s == nil || s.Version != 1 || s.Files == nil {
		return snapshot{}, 0, errors.New("unknown or missing local_directory cursor version")
	}
	round, err := strconv.ParseUint(s.Round, 10, 64)
	if err != nil || round == 0 || s.Round != strconv.FormatUint(round, 10) || len(s.Files) > maxFiles {
		return snapshot{}, 0, errors.New("invalid cursor round or file count")
	}
	for path, state := range s.Files {
		if !validPath(path) || !validHash(state.Digest) || !validHash(state.Revision) {
			return snapshot{}, 0, errors.New("invalid cursor file identity")
		}
	}
	return *s, round, nil
}

func encodeCursor(s snapshot) ([]byte, error) {
	var env cursorEnvelope
	env.ConnectorCursor.LocalDirectory = &s
	data, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	if len(data) > maxCursorBytes {
		return nil, errors.New("cursor snapshot exceeds size limit")
	}
	return data, nil
}

func validHash(s string) bool {
	if len(s) != sha256.Size*2 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func validPath(s string) bool {
	// Also reject characters unsafe in the current Host multipart filename.
	return s != "." && fs.ValidPath(s) && utf8.ValidString(s) && len(s) <= maxPathBytes &&
		!strings.ContainsAny(s, "\\\"") && !strings.ContainsFunc(s, unicode.IsControl)
}

func revision(round uint64, path, digest string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("local-directory-v1\x00%d\x00%s\x00%s", round, path, digest)))
	return hex.EncodeToString(h[:])
}
