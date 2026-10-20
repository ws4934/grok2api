package provider

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

// ErrInputUnavailable means a staged job input no longer exists.
var ErrInputUnavailable = errors.New("输入文件不存在或已过期")

// InputFile is an open staged input of a media job.
type InputFile struct {
	Body     io.ReadCloser
	MIMEType string
	Size     int64
}

// InputOpener opens the staged input a reference names.
type InputOpener func(ctx context.Context, reference string) (InputFile, error)

type inputOpenerKey struct{}

// WithInputOpener lets the adapters running under ctx read the staged inputs
// of their job. A job carries input references instead of input bytes, so an
// adapter reads a file only while it sends it upstream.
func WithInputOpener(ctx context.Context, open InputOpener) context.Context {
	return context.WithValue(ctx, inputOpenerKey{}, open)
}

// IsInputReference reports whether value names a staged input rather than
// carrying a URL or data URI.
func IsInputReference(value string) bool {
	_, ok := media.ParseInputReference(strings.TrimSpace(value))
	return ok
}

// OpenInput opens the staged input reference names; the caller closes it.
func OpenInput(ctx context.Context, reference string) (InputFile, error) {
	open, _ := ctx.Value(inputOpenerKey{}).(InputOpener)
	if open == nil {
		return InputFile{}, ErrInputUnavailable
	}
	return open(ctx, strings.TrimSpace(reference))
}

// ReadInput reads a staged input whole, for providers that parse or re-encode it.
func ReadInput(ctx context.Context, reference string) ([]byte, string, error) {
	file, err := OpenInput(ctx, reference)
	if err != nil {
		return nil, "", err
	}
	defer file.Body.Close()
	data, err := io.ReadAll(io.LimitReader(file.Body, media.MaxInputAssetBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrInputUnavailable, err)
	}
	if int64(len(data)) != file.Size {
		return nil, "", ErrInputUnavailable
	}
	return data, file.MIMEType, nil
}

// inputSlots bounds how many submissions hold staged inputs in memory at once.
// A submission holds one only until its request was sent, not while the job
// polls, so the bound costs a few seconds of waiting, not job concurrency.
var inputSlots = make(chan struct{}, 4)

// HoldInputs takes an input slot for a submission that reads its inputs into
// memory. Call release as soon as the request carrying them was sent; it is
// idempotent.
func HoldInputs(ctx context.Context) (release func(), err error) {
	select {
	case inputSlots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-inputSlots }) }, nil
	case <-ctx.Done():
		return func() {}, ctx.Err()
	}
}

// MaterializeInputs replaces the staged input references in a request body
// built from maps and slices with data URLs, for providers that embed inputs
// in JSON. When it replaced any, it holds an input slot (see HoldInputs) until
// release.
func MaterializeInputs(ctx context.Context, body any) (func(), error) {
	release, held := func() {}, false
	err := rewriteStrings(body, func(value string) (string, error) {
		if !IsInputReference(value) {
			return value, nil
		}
		if !held {
			var err error
			if release, err = HoldInputs(ctx); err != nil {
				return "", err
			}
			held = true
		}
		return inputDataURL(ctx, value)
	})
	if err != nil {
		release()
		return func() {}, err
	}
	return release, nil
}

// rewriteStrings replaces every string in a body built from maps and slices
// with what rewrite returns.
func rewriteStrings(value any, rewrite func(string) (string, error)) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if text, ok := item.(string); ok {
				next, err := rewrite(text)
				if err != nil {
					return err
				}
				typed[key] = next
				continue
			}
			if err := rewriteStrings(item, rewrite); err != nil {
				return err
			}
		}
	case []map[string]any:
		for _, item := range typed {
			if err := rewriteStrings(item, rewrite); err != nil {
				return err
			}
		}
	case []any:
		for index, item := range typed {
			if text, ok := item.(string); ok {
				next, err := rewrite(text)
				if err != nil {
					return err
				}
				typed[index] = next
				continue
			}
			if err := rewriteStrings(item, rewrite); err != nil {
				return err
			}
		}
	}
	return nil
}

// inputDataURL encodes a staged input into a data URL straight from its file.
func inputDataURL(ctx context.Context, reference string) (string, error) {
	file, err := OpenInput(ctx, reference)
	if err != nil {
		return "", err
	}
	defer file.Body.Close()
	var out strings.Builder
	prefix := "data:" + file.MIMEType + ";base64,"
	out.Grow(len(prefix) + base64.StdEncoding.EncodedLen(int(file.Size)))
	out.WriteString(prefix)
	encoder := base64.NewEncoder(base64.StdEncoding, &out)
	n, copyErr := io.Copy(encoder, io.LimitReader(file.Body, file.Size+1))
	if err := errors.Join(copyErr, encoder.Close()); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInputUnavailable, err)
	}
	if n != file.Size {
		return "", ErrInputUnavailable
	}
	return out.String(), nil
}
