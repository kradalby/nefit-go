package protocol

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"fmt"
	"html"
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

// BuildGetMessage constructs an HTTP GET request wrapped in an XMPP message stanza.
func BuildGetMessage(from, to, uri string) string {
	body := fmt.Sprintf("GET %s HTTP/1.1\rUser-Agent: NefitEasy\r\r", uri)
	return buildXMPPMessage(from, to, body)
}

// BuildPutMessage constructs an HTTP PUT request wrapped in an XMPP message stanza.
func BuildPutMessage(from, to, uri string, encryptedData string) string {
	body := fmt.Sprintf(
		"PUT %s HTTP/1.1\r"+
			"Content-Type: application/json\r"+
			"Content-Length: %d\r"+
			"User-Agent: NefitEasy\r"+
			"\r"+
			"%s",
		uri,
		len(encryptedData),
		encryptedData,
	)
	return buildXMPPMessage(from, to, body)
}

func buildXMPPMessage(from, to, body string) string {
	// Escape XML special characters in body, but preserve \r as &#13;\n for protocol
	escapedBody := escapeXMLBody(body)

	return fmt.Sprintf(
		`<message from="%s" to="%s"><body>%s</body></message>`,
		html.EscapeString(from),
		html.EscapeString(to),
		escapedBody,
	)
}

func escapeXMLBody(body string) string {
	placeholder := "\x00CRLF\x00"
	body = strings.ReplaceAll(body, "\r", placeholder)

	escaped := html.EscapeString(body)

	escaped = strings.ReplaceAll(escaped, placeholder, "&#13;\n")

	return escaped
}

// ParseHTTPResponse parses an HTTP-over-XMPP response.
func ParseHTTPResponse(data string) (*HTTPResponse, error) {
	// Replace &#13; entities back to \r for HTTP parsing
	data = strings.ReplaceAll(data, "&#13;", "\r")
	data = strings.ReplaceAll(data, "\n", "\r\n")

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

	body := msg.Body
	body = strings.ReplaceAll(body, "&#13;", "\r")

	return body, nil
}
