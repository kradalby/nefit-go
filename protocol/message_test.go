package protocol

import (
	"errors"
	"testing"
)

// Every device reply goes through ParseHTTPResponse, so its status-line and
// header handling is pinned here.

func TestParseHTTPResponse(t *testing.T) {
	tests := []struct {
		name        string
		data        string
		wantCode    int
		wantStatus  string
		wantCT      string
		wantBody    string
		wantHeaders map[string]string
	}{
		{
			name:       "json response",
			data:       "HTTP/1.1 200 OK&#13;\nContent-Type: application/json&#13;\nContent-Length: 14&#13;\n&#13;\n{\"value\":21.5}",
			wantCode:   200,
			wantStatus: "OK",
			wantCT:     "application/json",
			wantBody:   `{"value":21.5}`,
			wantHeaders: map[string]string{
				"Content-Type":   "application/json",
				"Content-Length": "14",
			},
		},
		{
			name:       "multi word status",
			data:       "HTTP/1.1 404 Not Found&#13;\n&#13;\n",
			wantCode:   404,
			wantStatus: "Not Found",
		},
		{
			name:       "no status text",
			data:       "HTTP/1.1 204&#13;\n&#13;\n",
			wantCode:   204,
			wantStatus: "",
		},
		{
			name:       "header value containing a colon",
			data:       "HTTP/1.1 200 OK&#13;\nX-Ref: a:b:c&#13;\n&#13;\n",
			wantCode:   200,
			wantStatus: "OK",
			wantHeaders: map[string]string{
				"X-Ref": "a:b:c",
			},
		},
		{
			name:       "empty header value",
			data:       "HTTP/1.1 200 OK&#13;\nX-Empty:&#13;\n&#13;\n",
			wantCode:   200,
			wantStatus: "OK",
			wantHeaders: map[string]string{
				"X-Empty": "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := ParseHTTPResponse(tt.data)
			if err != nil {
				t.Fatalf("ParseHTTPResponse() error = %v", err)
			}
			if resp.StatusCode != tt.wantCode {
				t.Errorf("StatusCode = %d, want %d", resp.StatusCode, tt.wantCode)
			}
			if resp.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", resp.Status, tt.wantStatus)
			}
			if tt.wantCT != "" && resp.ContentType != tt.wantCT {
				t.Errorf("ContentType = %q, want %q", resp.ContentType, tt.wantCT)
			}
			if tt.wantBody != "" && resp.Body != tt.wantBody {
				t.Errorf("Body = %q, want %q", resp.Body, tt.wantBody)
			}
			for k, want := range tt.wantHeaders {
				if got := resp.Headers[k]; got != want {
					t.Errorf("Headers[%q] = %q, want %q", k, got, want)
				}
			}
		})
	}
}

func TestParseHTTPResponseInvalid(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "no space in status line", data: "HTTP/1.1&#13;\n&#13;\n"},
		{name: "non numeric status code", data: "HTTP/1.1 NOPE OK&#13;\n&#13;\n"},
		{name: "empty input", data: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseHTTPResponse(tt.data); err == nil {
				t.Error("ParseHTTPResponse() expected an error, got nil")
			}
		})
	}
}

func TestExtractBodyRoundTrip(t *testing.T) {
	// A GET message built by this package must survive extraction with its
	// line separators intact, since ParseHTTPResponse depends on them.
	// Request text uses CRLF line endings, which survive the XML round trip.
	msg := BuildGetMessage("from@example.com", "to@example.com", "/heatingCircuits/hc1")

	body, err := ExtractBody(msg)
	if err != nil {
		t.Fatalf("ExtractBody() error = %v", err)
	}

	want := "GET /heatingCircuits/hc1 HTTP/1.1\r\nUser-Agent: NefitEasy\r\n\r\n"
	if body != want {
		t.Errorf("ExtractBody() = %q, want %q", body, want)
	}
}

func TestValidateURI(t *testing.T) {
	for _, uri := range []string{"/ecus/rrc/uiStatus", "/a?x=1&y=%20z", "/gateway/%75pdate"} {
		if err := ValidateURI(uri); err != nil {
			t.Errorf("%q rejected: %v", uri, err)
		}
	}
	for _, uri := range []string{"", "relative", "/a b", "/a\tb", "/a\rX-Inj: 1", "/a\nPUT /gateway/update", "/a\x00", "/a\x7f", "/café", "/a#frag"} {
		if err := ValidateURI(uri); !errors.Is(err, ErrInvalidURI) {
			t.Errorf("%q accepted: %v", uri, err)
		}
	}
}
