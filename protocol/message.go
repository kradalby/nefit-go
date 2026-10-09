package protocol

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// HTTPResponse represents a parsed HTTP-over-XMPP response.
type HTTPResponse struct {
	StatusCode  int
	Status      string
	Headers     map[string]string
	Body        string
	ContentType string
}

// ErrInvalidURI reports a request target that cannot be sent safely.
var ErrInvalidURI = errors.New("invalid request URI")

// ValidateURI accepts only an origin-form target ("/path?query") of visible
// ASCII. Whitespace or control bytes would end the request line and start new
// header lines or requests inside the HTTP-over-XMPP body; GetRequest,
// PutRequest and the Build functions do not check, so callers must. Percent-encoding and queries pass unchanged.
func ValidateURI(uri string) error {
	if !strings.HasPrefix(uri, "/") {
		return fmt.Errorf("%w %q: must start with /", ErrInvalidURI, uri)
	}
	for i := 0; i < len(uri); i++ {
		if b := uri[i]; b <= ' ' || b >= 0x7f || b == '#' {
			return fmt.Errorf("%w %q: byte %#x not allowed", ErrInvalidURI, uri, b)
		}
	}
	return nil
}

// GetRequest returns the HTTP-over-XMPP text of a GET for uri.
func GetRequest(uri string) string {
	return "GET " + uri + " HTTP/1.1\r\nUser-Agent: NefitEasy\r\n\r\n"
}

// PutRequest returns the HTTP-over-XMPP text of a PUT carrying encrypted data.
func PutRequest(uri, encryptedData string) string {
	return fmt.Sprintf("PUT %s HTTP/1.1\r\nContent-Type: application/json\r\nContent-Length: %d\r\nUser-Agent: NefitEasy\r\n\r\n%s",
		uri, len(encryptedData), encryptedData)
}

// BuildGetMessage constructs an HTTP GET request wrapped in an XMPP message stanza.
//
// Deprecated: check uri with ValidateURI, then xml.Marshal a MessageStanza
// whose Body is GetRequest(uri).
func BuildGetMessage(from, to, uri string) string {
	return buildXMPPMessage(from, to, GetRequest(uri))
}

// BuildPutMessage constructs an HTTP PUT request wrapped in an XMPP message stanza.
//
// Deprecated: check uri with ValidateURI, then xml.Marshal a MessageStanza
// whose Body is PutRequest(uri, encryptedData).
func BuildPutMessage(from, to, uri string, encryptedData string) string {
	return buildXMPPMessage(from, to, PutRequest(uri, encryptedData))
}

func buildXMPPMessage(from, to, body string) string {
	data, err := xml.Marshal(MessageStanza{From: from, To: to, Body: body})
	if err != nil {
		return ""
	}
	return string(data)
}

// ParseHTTPResponse parses an HTTP-over-XMPP response.
func ParseHTTPResponse(data string) (*HTTPResponse, error) {
	// Callers may pass text that was never XML-decoded; decoded text holds
	// this spelling only when a sender escaped CR twice.
	data = strings.ReplaceAll(data, "&#13;", "\r")
	data = strings.ReplaceAll(data, "\r\n", "\n")
	data = strings.ReplaceAll(data, "\r", "\n")

	reader := bufio.NewReader(strings.NewReader(data))

	statusLine, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read status line: %w", err)
	}

	statusLine = strings.TrimSpace(statusLine)
	_, rest, ok := strings.Cut(statusLine, " ")
	if !ok {
		return nil, fmt.Errorf("invalid status line: %s", statusLine)
	}

	codeStr, status, _ := strings.Cut(rest, " ")
	statusCode, err := strconv.Atoi(codeStr)
	if err != nil {
		return nil, fmt.Errorf("invalid status code: %s", codeStr)
	}

	headers := make(map[string]string)
	for {
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("failed to read header: %w", err)
		}

		line = strings.TrimSpace(line)
		if line == "" {
			break
		}

		if key, value, ok := strings.Cut(line, ":"); ok {
			headers[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}

		if err == io.EOF {
			break
		}
	}

	bodyBuf := new(bytes.Buffer)
	if _, err := io.Copy(bodyBuf, reader); err != nil && err != io.EOF {
		return nil, fmt.Errorf("failed to read body: %w", err)
	}

	contentType := headers["Content-Type"]

	return &HTTPResponse{
		StatusCode:  statusCode,
		Status:      status,
		Headers:     headers,
		Body:        bodyBuf.String(),
		ContentType: contentType,
	}, nil
}

// MessageStanza represents an XMPP message.
type MessageStanza struct {
	XMLName xml.Name `xml:"message"`
	From    string   `xml:"from,attr"`
	To      string   `xml:"to,attr"`
	Type    string   `xml:"type,attr,omitempty"`
	Body    string   `xml:"body"`
}

// ExtractBody extracts and decodes the body content from an XMPP message XML string.
func ExtractBody(xmlData string) (string, error) {
	var msg MessageStanza
	if err := xml.Unmarshal([]byte(xmlData), &msg); err != nil {
		return "", fmt.Errorf("failed to unmarshal message: %w", err)
	}

	// Decoded text holds &#13; only when a sender escaped CR twice.
	return strings.ReplaceAll(msg.Body, "&#13;", "\r"), nil
}
