package pipelinehttp

import (
	"bytes"
	"crypto/hmac"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const defaultSignatureSkew = 5 * time.Minute

// VerifyRequest authenticates an HMAC-signed request and returns its body. It
// also restores req.Body so the protocol handler can decode it afterwards.
// maxBody prevents an authenticated peer from forcing an unbounded allocation.
func VerifyRequest(req *http.Request, secret []byte, now time.Time, maxSkew time.Duration, maxBody int64) ([]byte, error) {
	if len(secret) == 0 {
		return nil, errors.New("pipeline plugin: HMAC secret is empty")
	}
	if maxSkew == 0 {
		maxSkew = defaultSignatureSkew
	}
	if maxSkew < 0 {
		return nil, errors.New("pipeline plugin: signature skew must not be negative")
	}
	if maxBody <= 0 {
		maxBody = defaultMaxResponseBytes
	}

	timestamp := req.Header.Get(TimestampHeader)
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return nil, errors.New("pipeline plugin: invalid signature timestamp")
	}
	signedAt := time.Unix(seconds, 0)
	if delta := now.Sub(signedAt); delta > maxSkew || delta < -maxSkew {
		return nil, errors.New("pipeline plugin: signature timestamp outside allowed skew")
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("pipeline plugin: read signed request: %w", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	if int64(len(body)) > maxBody {
		return nil, fmt.Errorf("pipeline plugin: request exceeds %d bytes", maxBody)
	}

	provided := req.Header.Get(SignatureHeader)
	encoded, ok := strings.CutPrefix(provided, "sha256=")
	if !ok {
		return nil, errors.New("pipeline plugin: invalid signature scheme")
	}
	expected := signature(secret, timestamp, req.Method, req.URL.EscapedPath(), body)
	if !hmac.Equal([]byte(encoded), []byte(expected)) {
		return nil, errors.New("pipeline plugin: signature mismatch")
	}
	return body, nil
}
