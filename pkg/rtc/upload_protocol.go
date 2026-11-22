package rtc

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type uploadProtocol struct {
	token      string
	totalBytes int
	buf        []byte
	ready      bool
	done       bool
	exitCode   int
	ackBytes   int
}

func generateUploadToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate upload token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func newUploadProtocol(token string, totalBytes int) (*uploadProtocol, error) {
	if len(token) < 16 {
		return nil, errors.New("token too short (must be at least 16 characters)")
	}
	if totalBytes < 0 {
		return nil, errors.New("totalBytes cannot be negative")
	}
	return &uploadProtocol{
		token:      token,
		totalBytes: totalBytes,
		exitCode:   -1,
	}, nil
}

func (p *uploadProtocol) Feed(data []byte) error {
	p.buf = append(p.buf, data...)

	markerPrefix := []byte("__MCP_UP_")
	markerSuffix := []byte("__")

	for {
		idx := bytes.Index(p.buf, markerPrefix)
		if idx < 0 {
			if len(p.buf) >= len(markerPrefix) {
				keepLen := len(markerPrefix) - 1
				p.buf = p.buf[len(p.buf)-keepLen:]
			}
			return nil
		}

		if idx > 0 {
			p.buf = p.buf[idx:]
		}

		endIdx := bytes.Index(p.buf[len(markerPrefix):], markerSuffix)
		// Check if another marker prefix appears before endIdx
		nextPrefix := bytes.Index(p.buf[len(markerPrefix):], markerPrefix)
		if nextPrefix >= 0 && (endIdx < 0 || nextPrefix < endIdx) {
			p.buf = p.buf[len(markerPrefix)+nextPrefix:]
			continue
		}

		// A valid marker cannot span across newlines
		nlIdx := bytes.IndexAny(p.buf[len(markerPrefix):], "\r\n")
		if nlIdx >= 0 && (endIdx < 0 || nlIdx < endIdx) {
			p.buf = p.buf[len(markerPrefix):]
			continue
		}

		if endIdx < 0 {
			if len(p.buf) > 1024 {
				p.buf = p.buf[len(markerPrefix):]
				continue
			}
			return nil
		}

		markerEnd := len(markerPrefix) + endIdx + len(markerSuffix)
		markerStr := string(p.buf[:markerEnd])
		p.buf = p.buf[markerEnd:]

		content := markerStr[len(markerPrefix) : len(markerStr)-len(markerSuffix)]

		if content == "READY_"+p.token {
			if p.done {
				return fmt.Errorf("received READY after DONE")
			}
			if p.ready {
				return fmt.Errorf("duplicate READY event received")
			}
			p.ready = true
			continue
		}

		ackPrefix := "ACK_" + p.token + "_"
		if strings.HasPrefix(content, ackPrefix) {
			if !p.ready {
				return fmt.Errorf("received ACK before READY")
			}
			if p.done {
				return fmt.Errorf("received ACK after DONE")
			}
			numStr := strings.TrimPrefix(content, ackPrefix)
			offset, err := strconv.Atoi(numStr)
			if err != nil || offset < 0 {
				return fmt.Errorf("malformed ACK event: invalid offset %q", numStr)
			}
			if offset <= p.ackBytes && p.ackBytes > 0 {
				return fmt.Errorf("non-monotonic ACK offset: %d <= %d", offset, p.ackBytes)
			}
			if offset > p.totalBytes {
				return fmt.Errorf("ACK offset %d exceeds total bytes %d", offset, p.totalBytes)
			}
			p.ackBytes = offset
			continue
		}

		donePrefix := "DONE_" + p.token + "_"
		if strings.HasPrefix(content, donePrefix) {
			if !p.ready {
				return fmt.Errorf("received DONE before READY")
			}
			if p.done {
				return fmt.Errorf("duplicate DONE event received")
			}
			codeStr := strings.TrimPrefix(content, donePrefix)
			code, err := strconv.Atoi(codeStr)
			if err != nil {
				return fmt.Errorf("malformed DONE event: invalid exit code %q", codeStr)
			}
			if code < 0 || code > 255 {
				return fmt.Errorf("malformed DONE event: out of range exit code %d", code)
			}
			if code == 0 && p.totalBytes > 0 && p.ackBytes < p.totalBytes {
				return fmt.Errorf("premature DONE with exit code 0 when bytes remain unacknowledged (%d < %d)", p.ackBytes, p.totalBytes)
			}
			p.done = true
			p.exitCode = code
			continue
		}

		if strings.Contains(content, p.token) {
			return fmt.Errorf("malformed protocol event for token: %s", content)
		}

		// Marker with different token / unrelated text; ignore
		continue
	}
}

func (p *uploadProtocol) IsReady() bool {
	return p.ready
}

func (p *uploadProtocol) AcknowledgedBytes() int {
	return p.ackBytes
}

func (p *uploadProtocol) IsDone() bool {
	return p.done
}

func (p *uploadProtocol) ExitCode() int {
	return p.exitCode
}

func (p *uploadProtocol) Token() string {
	return p.token
}

func (p *uploadProtocol) TotalBytes() int {
	return p.totalBytes
}
