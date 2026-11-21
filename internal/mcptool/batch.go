package mcptool

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
)

// The MCP protocol versions that decide whether a body may be a JSON-RPC
// batch. 2025-03-26 requires a server to receive batches (basic/index,
// "Batching": "MUST support receiving JSON-RPC batches"); 2025-06-18
// removed them (changelog: "Remove support for JSON-RPC batching"), and
// every later version keeps them out. A request that names no version is
// read as 2025-03-26 (2025-06-18 basic/transports, "Protocol Version
// Header"), as the SDK reads it.
const (
	batchesRemovedIn = "2025-06-18"
	protocolHeader   = "Mcp-Protocol-Version"
)

// MaxBatchMessages bounds a batch where the protocol still allows one.
// The spec sets no size, and none was seen: production served no batch
// in the 14 days to 2026-11-21 (276 POSTs to /mcp, each one message).
// The per-caller rate limit counts requests, not the messages in them,
// so a batch is a way past it; eight messages is room for a client
// that batches a turn's few calls, and no more.
const MaxBatchMessages = 8

// ReadBody reads a POST's body, bounded by maxBody as the MCP handler
// bounds it, for a middleware in front of that handler. It answers r
// itself, and returns false, for a body it cannot read (413 past
// maxBody) and for a JSON-RPC batch r's protocol version does not
// allow, or one of more than MaxBatchMessages messages (400). It reads a
// batch as the MCP SDK's streamable handler does (a top-level array; the
// version is r's Mcp-Protocol-Version header) and decodes none of it, so
// a refused batch is never decoded, by the SDK or by the gateway's own
// readers. Every other body is the SDK's to read, or to refuse.
func ReadBody(
	w http.ResponseWriter, r *http.Request, maxBody int64,
) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		httpmw.WriteJSONError(w, http.StatusRequestEntityTooLarge,
			"request body too large")
		return nil, false
	}
	if err != nil {
		httpmw.WriteJSONError(w, http.StatusBadRequest,
			"request body could not be read")
		return nil, false
	}
	if refuseBatch(w, r, body) {
		return nil, false
	}
	return body, true
}

// refuseBatch answers r with a 400, and returns true, for a batch its
// protocol version does not allow or that holds too many messages.
func refuseBatch(w http.ResponseWriter, r *http.Request, body []byte) bool {
	n, batch := batchMessages(body)
	if !batch {
		return false
	}
	version := r.Header.Get(protocolHeader)
	switch {
	case version >= batchesRemovedIn:
		httpmw.WriteJSONError(w, http.StatusBadRequest, fmt.Sprintf(
			"JSON-RPC batching is not supported in %s and later "+
				"(request version: %s)", batchesRemovedIn, version))
	case n > MaxBatchMessages:
		httpmw.WriteJSONError(w, http.StatusBadRequest, fmt.Sprintf(
			"a JSON-RPC batch carries at most %d messages",
			MaxBatchMessages))
	default:
		return false
	}
	return true
}

// batchMessages reports whether body is a JSON array, which the SDK
// reads as a batch, and how many elements its top level holds, counted
// by a scan that decodes nothing: a value begins at the array's own level
// on the first byte that is not a space, a comma or its closing bracket,
// outside strings. A body that is not JSON is the SDK's to refuse.
func batchMessages(body []byte) (int, bool) {
	body = bytes.TrimLeft(body, " \t\r\n")
	if len(body) == 0 || body[0] != '[' {
		return 0, false
	}
	n, depth := 0, 1
	inString, escaped, inValue := false, false, false
	for _, b := range body[1:] {
		if inString {
			switch {
			case escaped:
				escaped = false
			case b == '\\':
				escaped = true
			case b == '"':
				inString = false
			}
			continue
		}
		if depth == 1 {
			switch b {
			case ' ', '\t', '\r', '\n':
				continue
			case ',':
				inValue = false
				continue
			case ']':
				return n, true
			}
			if !inValue {
				n++
				inValue = true
			}
		}
		switch b {
		case '"':
			inString = true
		case '[', '{':
			depth++
		case ']', '}':
			depth--
		}
	}
	return n, true
}
