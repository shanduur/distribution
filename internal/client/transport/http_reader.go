package transport

import (
	"compress/flate"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/klauspost/compress/zstd"
)

var (
	contentRangeRegexp = regexp.MustCompile(`bytes ([0-9]+)-([0-9]+)/([0-9]+|\\*)`)

	// ErrWrongCodeForByteRange is returned if the client sends a request
	// with a Range header but the server returns a 2xx or 3xx code other
	// than 206 Partial Content.
	ErrWrongCodeForByteRange = errors.New("expected HTTP 206 from byte range request")
)

// NewHTTPReadSeeker handles reading from an HTTP endpoint using a GET
// request. When seeking and starting a read from a non-zero offset
// the a "Range" header will be added which sets the offset.
//
// TODO(dmcgowan): Move this into a separate utility package
func NewHTTPReadSeeker(ctx context.Context, client *http.Client, url string, errorHandler func(*http.Response) error) *HTTPReadSeeker {
	return &HTTPReadSeeker{
		ctx:          ctx,
		client:       client,
		url:          url,
		errorHandler: errorHandler,
	}
}

// HTTPReadSeeker implements an [io.ReadSeekCloser].
type HTTPReadSeeker struct {
	ctx    context.Context
	client *http.Client
	url    string

	// errorHandler creates an error from an unsuccessful HTTP response.
	// This allows the error to be created with the HTTP response body
	// without leaking the body through a returned error.
	errorHandler func(*http.Response) error

	size int64

	// rc is the remote read closer.
	rc io.ReadCloser
	// readerOffset tracks the offset as of the last read.
	readerOffset int64
	// seekOffset allows Seek to override the offset. Seek changes
	// seekOffset instead of changing readOffset directly so that
	// connection resets can be delayed and possibly avoided if the
	// seek is undone (i.e. seeking to the end and then back to the
	// beginning).
	seekOffset int64
	err        error
}

func (hrs *HTTPReadSeeker) Read(p []byte) (n int, err error) {
	if hrs.err != nil {
		return 0, hrs.err
	}

	// If we sought to a different position, we need to reset the
	// connection. This logic is here instead of Seek so that if
	// a seek is undone before the next read, the connection doesn't
	// need to be closed and reopened. A common example of this is
	// seeking to the end to determine the length, and then seeking
	// back to the original position.
	if hrs.readerOffset != hrs.seekOffset {
		hrs.reset()
	}

	hrs.readerOffset = hrs.seekOffset

	rd, err := hrs.reader()
	if err != nil {
		return 0, err
	}

	n, err = rd.Read(p)
	hrs.seekOffset += int64(n)
	hrs.readerOffset += int64(n)

	return n, err
}

func (hrs *HTTPReadSeeker) Seek(offset int64, whence int) (int64, error) {
	if hrs.err != nil {
		return 0, hrs.err
	}

	lastReaderOffset := hrs.readerOffset

	if whence == io.SeekStart && hrs.rc == nil {
		// If no request has been made yet, and we are seeking to an
		// absolute position, set the read offset as well to avoid an
		// unnecessary request.
		hrs.readerOffset = offset
	}

	_, err := hrs.reader()
	if err != nil {
		hrs.readerOffset = lastReaderOffset
		return 0, err
	}

	newOffset := hrs.seekOffset

	switch whence {
	case io.SeekCurrent:
		newOffset += offset
	case io.SeekEnd:
		if hrs.size < 0 {
			return 0, errors.New("content length not known")
		}
		newOffset = hrs.size + offset
	case io.SeekStart:
		newOffset = offset
	}

	if newOffset < 0 {
		err = errors.New("cannot seek to negative position")
	} else {
		hrs.seekOffset = newOffset
	}

	return hrs.seekOffset, err
}

func (hrs *HTTPReadSeeker) Close() error {
	if hrs.err != nil {
		return hrs.err
	}

	// close and release reader chain
	if hrs.rc != nil {
		hrs.rc.Close()
	}

	hrs.rc = nil

	hrs.err = errors.New("httpLayer: closed")

	return nil
}

func (hrs *HTTPReadSeeker) reset() {
	if hrs.err != nil {
		return
	}
	if hrs.rc != nil {
		hrs.rc.Close()
		hrs.rc = nil
	}
}

func (hrs *HTTPReadSeeker) reader() (_ io.Reader, retErr error) {
	if hrs.err != nil {
		return nil, hrs.err
	}

	if hrs.rc != nil {
		return hrs.rc, nil
	}

	req, err := http.NewRequestWithContext(hrs.ctx, http.MethodGet, hrs.url, nil)
	if err != nil {
		return nil, err
	}

	if hrs.readerOffset > 0 {
		// If we are at different offset, issue a range request from there.
		req.Header.Add("Range", fmt.Sprintf("bytes=%d-", hrs.readerOffset))
		// TODO: get context in here
		// context.GetLogger(hrs.context).Infof("Range: %s", req.Header.Get("Range"))
	}

	req.Header.Add("Accept-Encoding", "zstd, gzip, deflate")
	resp, err := hrs.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = resp.Body.Close()
		}
	}()

	// Normally would use client.SuccessStatus, but that would be a cyclic
	// import
	if resp.StatusCode >= 200 && resp.StatusCode <= 399 {
		if hrs.readerOffset > 0 {
			if resp.StatusCode != http.StatusPartialContent {
				return nil, ErrWrongCodeForByteRange
			}

			contentRange := resp.Header.Get("Content-Range")
			if contentRange == "" {
				return nil, errors.New("no Content-Range header found in HTTP 206 response")
			}

			submatches := contentRangeRegexp.FindStringSubmatch(contentRange)
			if len(submatches) < 4 {
				return nil, fmt.Errorf("could not parse Content-Range header: %s", contentRange)
			}

			startByte, err := strconv.ParseUint(submatches[1], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("could not parse start of range in Content-Range header: %s", contentRange)
			}

			if startByte != uint64(hrs.readerOffset) {
				return nil, fmt.Errorf("received Content-Range starting at offset %d instead of requested %d", startByte, hrs.readerOffset)
			}

			endByte, err := strconv.ParseUint(submatches[2], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("could not parse end of range in Content-Range header: %s", contentRange)
			}

			if submatches[3] == "*" {
				hrs.size = -1
			} else {
				size, err := strconv.ParseUint(submatches[3], 10, 64)
				if err != nil {
					return nil, fmt.Errorf("could not parse total size in Content-Range header: %s", contentRange)
				}

				if endByte+1 != size {
					return nil, fmt.Errorf("range in Content-Range stops before the end of the content: %s", contentRange)
				}

				if size > math.MaxInt64 {
					return nil, fmt.Errorf("Content-Range size: %d exceeds max allowed size", size)
				}
				hrs.size = int64(size)
			}
		} else if resp.StatusCode == http.StatusOK {
			hrs.size = resp.ContentLength
		} else {
			hrs.size = -1
		}

		body := resp.Body
		encoding := strings.FieldsFunc(resp.Header.Get("Content-Encoding"), func(r rune) bool {
			return unicode.IsSpace(r) || r == ','
		})
		for i := len(encoding) - 1; i >= 0; i-- {
			algorithm := strings.ToLower(encoding[i])
			switch algorithm {
			case "zstd":
				r, err := zstd.NewReader(body)
				if err != nil {
					return nil, err
				}
				body = r.IOReadCloser()
			case "gzip":
				body, err = gzip.NewReader(body)
				if err != nil {
					return nil, err
				}
			case "deflate":
				body = flate.NewReader(body)
			case "":
				// no content-encoding applied, use raw body
			default:
				return nil, errors.New("unsupported Content-Encoding algorithm: " + algorithm)
			}
		}

		hrs.rc = body
	} else {
		if hrs.errorHandler != nil {
			// Closing the body should be handled by the existing defer,
			// but in case a custom "errHandler" is used that doesn't return
			// an error, we close the body regardless.
			defer resp.Body.Close()
			return nil, hrs.errorHandler(resp)
		}
		return nil, fmt.Errorf("unexpected status resolving reader: %v", resp.Status)
	}

	return hrs.rc, nil
}
