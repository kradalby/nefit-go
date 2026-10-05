package protocol

import (
	"bufio"
	"bytes"
	"encoding/xml"
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
	data, err := xml.Marshal(MessageStanza{From: from, To: to, Body: strings.ReplaceAll(body, "\r", "\r\n")})
	if err != nil {
		return ""
	}
	return string(data)
}

// ParseHTTPResponse parses an HTTP-over-XMPP response.
func ParseHTTPResponse(data string) (*HTTPResponse, error) {
	// Replace &#13; entities back to \r for HTTP parsing
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

// MarshalXML preserves the namespace learned from the incoming stream.
func (m MessageStanza) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Name = m.XMLName
	if start.Name.Local == "" {
		start.Name.Local = "message"
	}
	return e.EncodeElement(struct {
		From string `xml:"from,attr"`
		To   string `xml:"to,attr"`
		Type string `xml:"type,attr,omitempty"`
		Body string `xml:"body"`
	}{m.From, m.To, m.Type, m.Body}, start)
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
