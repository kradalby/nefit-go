package xmpp

import (
	"encoding/xml"
)

// Body contains the HTTP-over-XMPP text. DeviceXML adapts the encoded newline
// spelling for the thermostat while retaining typed XML marshaling.
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

func Auth(mechanism string) SASL {
	return SASL{XMLName: xml.Name{Space: SASLNS, Local: "auth"}, Mechanism: mechanism}
}

func SASLText(kind, text string) SASL {
	return SASL{XMLName: xml.Name{Space: SASLNS, Local: kind}, Text: text}
}

type (
	Mechanisms struct {
		Values []string `xml:"urn:ietf:params:xml:ns:xmpp-sasl mechanism"`
	}
	Features struct {
		XMLName    xml.Name    `xml:"http://etherx.jabber.org/streams features"`
		Mechanisms *Mechanisms `xml:"urn:ietf:params:xml:ns:xmpp-sasl mechanisms"`
		Bind       *Element    `xml:"urn:ietf:params:xml:ns:xmpp-bind bind"`
		Session    *Element    `xml:"urn:ietf:params:xml:ns:xmpp-session session"`
		TLS        *Element    `xml:"urn:ietf:params:xml:ns:xmpp-tls starttls"`
		Extensions []Element   `xml:",any"`
	}
)

// Typed decodes a preserved stanza into its known protocol representation.
func (e Element) Typed() (any, error) {
	raw, err := xml.Marshal(e)
	if err != nil {
		return nil, err
	}
	var v any
	switch e.Name {
	case xml.Name{Space: ClientNS, Local: "message"}:
		v = new(Message)
	case xml.Name{Space: ClientNS, Local: "iq"}:
		v = new(IQ)
	case xml.Name{Space: ClientNS, Local: "presence"}:
		v = new(Presence)
	case xml.Name{Space: StreamNS, Local: "features"}:
		v = new(Features)
	default:
		if e.Name.Space == SASLNS {
			v = new(SASL)
		} else {
			return &e, nil
		}
	}
	return v, xml.Unmarshal(raw, v)
}

// encoding/xml matches unqualified attribute tags by local name. Separate
// foreign attributes before decoding so they cannot replace routing fields.
func typedAttributes(start xml.StartElement) (xml.StartElement, []xml.Attr, error) {
	if err := validateAttributes(start.Attr); err != nil {
		return start, nil, err
	}
	var attrs, foreign []xml.Attr
	for _, a := range start.Attr {
		if namespaceAttribute(a.Name) {
			continue
		}
		if a.Name.Space == "" || a.Name == (xml.Name{Space: XMLNS, Local: "lang"}) {
			attrs = append(attrs, a)
		} else {
			foreign = append(foreign, a)
		}
	}
	start.Attr = attrs
	return start, foreign, nil
}

func (m *Message) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	start, foreign, err := typedAttributes(start)
	if err != nil {
		return err
	}
	type plain Message
	var value plain
	if err := d.DecodeElement(&value, &start); err != nil {
		return err
	}
	value.Attr = append(value.Attr, foreign...)
	*m = Message(value)
	return nil
}

func (m *IQ) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	start, foreign, err := typedAttributes(start)
	if err != nil {
		return err
	}
	type plain IQ
	var value plain
	if err := d.DecodeElement(&value, &start); err != nil {
		return err
	}
	value.Attr = append(value.Attr, foreign...)
	*m = IQ(value)
	return nil
}

func (m *Presence) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	start, foreign, err := typedAttributes(start)
	if err != nil {
		return err
	}
	type plain Presence
	var value plain
	if err := d.DecodeElement(&value, &start); err != nil {
		return err
	}
	value.Attr = append(value.Attr, foreign...)
	*m = Presence(value)
	return nil
}

func (m *SASL) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	start, foreign, err := typedAttributes(start)
	if err != nil {
		return err
	}
	type plain SASL
	var value plain
	if err := d.DecodeElement(&value, &start); err != nil {
		return err
	}
	value.Attr = append(value.Attr, foreign...)
	*m = SASL(value)
	return nil
}
