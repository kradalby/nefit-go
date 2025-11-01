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

// HTTPResponse represents a parsed HTTP-over-XMPP response
type HTTPResponse struct {
	StatusCode  int
	Status      string
	Headers     map[string]string
	Body        string
	ContentType string
}

// BuildGetMessage creates an HTTP GET request formatted for XMPP
func BuildGetMessage(from, to, uri string) string {
	body := fmt.Sprintf("GET %s HTTP/1.1\rUser-Agent: NefitEasy\r\r", uri)
	return buildXMPPMessage(from, to, body)
}

// BuildPutMessage creates an HTTP PUT request formatted for XMPP
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

// buildXMPPMessage wraps HTTP-style body in an XMPP message stanza
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

// escapeXMLBody escapes XML content but preserves \r as &#13;\n
func escapeXMLBody(body string) string {
	// Replace \r with placeholder first
	placeholder := "\x00CRLF\x00"
	body = strings.ReplaceAll(body, "\r", placeholder)

	// Escape XML
	escaped := html.EscapeString(body)

	// Replace placeholder with &#13;\n
	escaped = strings.ReplaceAll(escaped, placeholder, "&#13;\n")

	return escaped
}

// ParseHTTPResponse parses an HTTP-over-XMPP response
func ParseHTTPResponse(data string) (*HTTPResponse, error) {
	// Replace &#13; entities back to \r for HTTP parsing
	data = strings.ReplaceAll(data, "&#13;", "\r")
	data = strings.ReplaceAll(data, "\n", "\r\n")

	reader := bufio.NewReader(strings.NewReader(data))

	// Parse status line
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read status line: %w", err)
	}

	statusLine = strings.TrimSpace(statusLine)
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid status line: %s", statusLine)
	}

	statusCode, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid status code: %s", parts[1])
	}

	status := ""
	if len(parts) == 3 {
		status = parts[2]
	}

	// Parse headers
	headers := make(map[string]string)
	for {
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("failed to read header: %w", err)
		}

		line = strings.TrimSpace(line)
		if line == "" {
			break // End of headers
		}

		// Parse header
		headerParts := strings.SplitN(line, ":", 2)
		if len(headerParts) == 2 {
			key := strings.TrimSpace(headerParts[0])
			value := strings.TrimSpace(headerParts[1])
			headers[key] = value
		}

		if err == io.EOF {
			break
		}
	}

	// Read body (rest of the data)
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

// MessageStanza represents an XMPP message
type MessageStanza struct {
	XMLName xml.Name `xml:"message"`
	From    string   `xml:"from,attr"`
	To      string   `xml:"to,attr"`
	Type    string   `xml:"type,attr,omitempty"`
	Body    string   `xml:"body"`
}

// ExtractBody extracts the body from an XMPP message XML string
func ExtractBody(xmlData string) (string, error) {
	var msg MessageStanza
	if err := xml.Unmarshal([]byte(xmlData), &msg); err != nil {
		return "", fmt.Errorf("failed to unmarshal message: %w", err)
	}

	// Decode HTML entities
	body := msg.Body
	body = strings.ReplaceAll(body, "&#13;", "\r")

	return body, nil
}
