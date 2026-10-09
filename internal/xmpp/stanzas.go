package xmpp

import (
	"bytes"
	"encoding/xml"
	"errors"
)

// Body contains the HTTP-over-XMPP text. Marshal's device form adapts the
// encoded newline spelling for the thermostat while retaining typed XML
// marshaling.
type (
	Body struct {
		Text string `xml:",chardata"`
	}
	Message struct {
		XMLName    xml.Name   `xml:"jabber:client message"`
		From       string     `xml:"from,attr,omitempty"`
		To         string     `xml:"to,attr,omitempty"`
		Type       string     `xml:"type,attr,omitempty"`
		ID         string     `xml:"id,attr,omitempty"`
		Lang       string     `xml:"http://www.w3.org/XML/1998/namespace lang,attr,omitempty"`
		Body       Body       `xml:"jabber:client body"`
		Extensions []Element  `xml:",any"`
		Attr       []xml.Attr `xml:",any,attr"`
	}
)

type (
	Bind struct {
		Resource string `xml:"urn:ietf:params:xml:ns:xmpp-bind resource,omitempty"`
		JID      string `xml:"urn:ietf:params:xml:ns:xmpp-bind jid,omitempty"`
	}
	IQ struct {
		XMLName    xml.Name   `xml:"jabber:client iq"`
		Type       string     `xml:"type,attr,omitempty"`
		ID         string     `xml:"id,attr,omitempty"`
		From       string     `xml:"from,attr,omitempty"`
		To         string     `xml:"to,attr,omitempty"`
		Bind       *Bind      `xml:"urn:ietf:params:xml:ns:xmpp-bind bind"`
		Session    *Element   `xml:"urn:ietf:params:xml:ns:xmpp-session session"`
		Extensions []Element  `xml:",any"`
		Attr       []xml.Attr `xml:",any,attr"`
	}
)

type Presence struct {
	XMLName    xml.Name   `xml:"jabber:client presence"`
	From       string     `xml:"from,attr,omitempty"`
	To         string     `xml:"to,attr,omitempty"`
	Type       string     `xml:"type,attr,omitempty"`
	ID         string     `xml:"id,attr,omitempty"`
	Status     string     `xml:"jabber:client status,omitempty"`
	Extensions []Element  `xml:",any"`
	Attr       []xml.Attr `xml:",any,attr"`
}
type SASL struct {
	XMLName    xml.Name
	Mechanism  string     `xml:"mechanism,attr,omitempty"`
	Text       string     `xml:",chardata"`
	Extensions []Element  `xml:",any"`
	Attr       []xml.Attr `xml:",any,attr"`
}

func SASLText(kind, text string) SASL {
	return SASL{XMLName: xml.Name{Space: SASLNS, Local: kind}, Text: text}
}

// Typed decodes a preserved stanza into its known protocol representation.
// Re-encoding is bounded: namespace declarations repeated per element could
// otherwise multiply a small hostile stanza.
func (e Element) Typed() (any, error) {
	var v any
	switch e.Name {
	case xml.Name{Space: ClientNS, Local: "message"}:
		v = new(Message)
	case xml.Name{Space: ClientNS, Local: "iq"}:
		v = new(IQ)
	case xml.Name{Space: ClientNS, Local: "presence"}:
		v = new(Presence)
	default:
		if e.Name.Space != SASLNS {
			return &e, nil
		}
		v = new(SASL)
	}
	raw := limitedBuffer{limit: 2 * MaxStanzaBytes}
	if err := xml.NewEncoder(&raw).Encode(e); err != nil {
		return nil, err
	}
	return v, xml.Unmarshal(raw.Bytes(), v)
}

// limitedBuffer fails writes past limit instead of growing.
type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errors.New("encoded XMPP stanza exceeds size limit")
	}
	return b.Buffer.Write(p)
}

// encoding/xml matches unqualified attribute tags by local name. Separate
// foreign attributes before decoding so they cannot replace routing fields.
func decodeTyped(d *xml.Decoder, start xml.StartElement, value any, attrs *[]xml.Attr) error {
	if err := validateAttributes(start.Attr); err != nil {
		return err
	}
	var known, foreign []xml.Attr
	for _, a := range start.Attr {
		switch {
		case namespaceAttribute(a.Name):
		case a.Name.Space == "" || a.Name == (xml.Name{Space: XMLNS, Local: "lang"}):
			known = append(known, a)
		default:
			foreign = append(foreign, a)
		}
	}
	start.Attr = known
	if err := d.DecodeElement(value, &start); err != nil {
		return err
	}
	*attrs = append(*attrs, foreign...)
	return nil
}

func (m *Message) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	type plain Message
	return decodeTyped(d, start, (*plain)(m), &m.Attr)
}

func (m *IQ) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	type plain IQ
	return decodeTyped(d, start, (*plain)(m), &m.Attr)
}

func (m *Presence) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	type plain Presence
	return decodeTyped(d, start, (*plain)(m), &m.Attr)
}

func (m *SASL) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	type plain SASL
	return decodeTyped(d, start, (*plain)(m), &m.Attr)
}
