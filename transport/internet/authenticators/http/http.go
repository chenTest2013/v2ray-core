package http

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"time"

	"v2ray.com/core/common/alloc"
	"v2ray.com/core/common/loader"
	"v2ray.com/core/transport/internet"
)

const (
	CRLF   = "\r\n"
	ENDING = CRLF + CRLF
)

type Reader interface {
	Read(io.Reader) (*alloc.Buffer, error)
}

type Writer interface {
	Write(io.Writer) error
}

type NoOpReader struct{}

func (v *NoOpReader) Read(io.Reader) (*alloc.Buffer, error) {
	return nil, nil
}

type NoOpWriter struct{}

func (v *NoOpWriter) Write(io.Writer) error {
	return nil
}

type HeaderReader struct {
}

func (*HeaderReader) Read(reader io.Reader) (*alloc.Buffer, error) {
	buffer := alloc.NewSmallBuffer().Clear()
	for {
		_, err := buffer.FillFrom(reader)
		if err != nil {
			return nil, err
		}
		if n := bytes.Index(buffer.Value, []byte(ENDING)); n != -1 {
			buffer.SliceFrom(n + len(ENDING))
			break
		}
		if buffer.Len() >= len(ENDING) {
			copy(buffer.Value, buffer.Value[buffer.Len()-len(ENDING):])
			buffer.Slice(0, len(ENDING))
		}
	}
	if buffer.IsEmpty() {
		buffer.Release()
		return nil, nil
	}
	return buffer, nil
}

type HeaderWriter struct {
	header *alloc.Buffer
}

func NewHeaderWriter(header *alloc.Buffer) *HeaderWriter {
	return &HeaderWriter{
		header: header,
	}
}

func (v *HeaderWriter) Write(writer io.Writer) error {
	if v.header == nil {
		return nil
	}
	_, err := writer.Write(v.header.Value)
	v.header.Release()
	v.header = nil
	return err
}

type HttpConn struct {
	net.Conn

	readBuffer    *alloc.Buffer
	oneTimeReader Reader
	oneTimeWriter Writer
}

func NewHttpConn(conn net.Conn, reader Reader, writer Writer) *HttpConn {
	return &HttpConn{
		Conn:          conn,
		oneTimeReader: reader,
		oneTimeWriter: writer,
	}
}

func (v *HttpConn) Read(b []byte) (int, error) {
	if v.oneTimeReader != nil {
		buffer, err := v.oneTimeReader.Read(v.Conn)
		if err != nil {
			return 0, err
		}
		v.readBuffer = buffer
		v.oneTimeReader = nil
	}

	if v.readBuffer.Len() > 0 {
		nBytes, err := v.readBuffer.Read(b)
		if nBytes == v.readBuffer.Len() {
			v.readBuffer.Release()
			v.readBuffer = nil
		}
		return nBytes, err
	}

	return v.Conn.Read(b)
}

func (v *HttpConn) Write(b []byte) (int, error) {
	if v.oneTimeWriter != nil {
		err := v.oneTimeWriter.Write(v.Conn)
		v.oneTimeWriter = nil
		if err != nil {
			return 0, err
		}
	}

	return v.Conn.Write(b)
}

type HttpAuthenticator struct {
	config *Config
}

func (v HttpAuthenticator) GetClientWriter() *HeaderWriter {
	header := alloc.NewSmallBuffer().Clear()
	config := v.config.Request
	header.AppendString(config.Method.GetValue()).AppendString(" ").AppendString(config.PickUri()).AppendString(" ").AppendString(config.GetFullVersion()).AppendString(CRLF)

	headers := config.PickHeaders()
	for _, h := range headers {
		header.AppendString(h).AppendString(CRLF)
	}
	header.AppendString(CRLF)
	return &HeaderWriter{
		header: header,
	}
}

func (v HttpAuthenticator) GetServerWriter() *HeaderWriter {
	header := alloc.NewSmallBuffer().Clear()
	config := v.config.Response
	header.AppendString(config.GetFullVersion()).AppendString(" ").AppendString(config.Status.GetCode()).AppendString(" ").AppendString(config.Status.GetReason()).AppendString(CRLF)

	headers := config.PickHeaders()
	for _, h := range headers {
		header.AppendString(h).AppendString(CRLF)
	}
	if !config.HasHeader("Date") {
		header.AppendString("Date: ").AppendString(time.Now().Format(http.TimeFormat)).AppendString(CRLF)
	}
	header.AppendString(CRLF)
	return &HeaderWriter{
		header: header,
	}
}

func (v HttpAuthenticator) Client(conn net.Conn) net.Conn {
	if v.config.Request == nil && v.config.Response == nil {
		return conn
	}
	var reader Reader = new(NoOpReader)
	if v.config.Request != nil {
		reader = new(HeaderReader)
	}

	var writer Writer = new(NoOpWriter)
	if v.config.Response != nil {
		writer = v.GetClientWriter()
	}
	return NewHttpConn(conn, reader, writer)
}

func (v HttpAuthenticator) Server(conn net.Conn) net.Conn {
	if v.config.Request == nil && v.config.Response == nil {
		return conn
	}
	return NewHttpConn(conn, new(HeaderReader), v.GetServerWriter())
}

type HttpAuthenticatorFactory struct{}

func (HttpAuthenticatorFactory) Create(config interface{}) internet.ConnectionAuthenticator {
	return HttpAuthenticator{
		config: config.(*Config),
	}
}

func init() {
	internet.RegisterConnectionAuthenticator(loader.GetType(new(Config)), HttpAuthenticatorFactory{})
}
