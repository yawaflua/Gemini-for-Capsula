package httpapi

import (
	"context"
	_ "embed"
	"io"
	"net/http"
	"strings"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	projectv1 "github.com/yawaflua/marusya-gemini/registry/backend/gen/project/v1"
	"github.com/yawaflua/marusya-gemini/registry/backend/internal/auth"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

//go:embed swagger.html
var swaggerUI []byte

// NewHandler serves the API under /v1/ and, if siteDir is set, the built
// frontend from it on every other path.
func NewHandler(service projectv1.PluginRegistryServer, authenticator *auth.Authenticator, siteDir string) http.Handler {
	jsonMarshaler := &runtime.JSONPb{
		MarshalOptions: protojson.MarshalOptions{
			UseProtoNames:   true,
			EmitUnpopulated: true,
		},
		UnmarshalOptions: protojson.UnmarshalOptions{
			DiscardUnknown: false,
		},
	}

	gateway := runtime.NewServeMux(
		runtime.WithIncomingHeaderMatcher(incomingHeaderMatcher),
		runtime.WithOutgoingHeaderMatcher(outgoingHeaderMatcher),
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.HTTPBodyMarshaler{Marshaler: jsonMarshaler}),
		runtime.WithMarshalerOption("application/zip", newRawHTTPBodyMarshaler("application/zip", jsonMarshaler)),
		runtime.WithMarshalerOption("application/octet-stream", newRawHTTPBodyMarshaler("application/octet-stream", jsonMarshaler)),
	)
	if err := projectv1.RegisterPluginRegistryHandlerServer(context.Background(), gateway, service); err != nil {
		panic(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/", authenticator.HTTPMiddleware(gateway))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /swagger.json", serveSwaggerJSON)
	mux.HandleFunc("GET /swagger/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(swaggerUI)
	})
	if siteDir != "" {
		mux.Handle("/", siteHandler(siteDir))
	} else {
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/swagger/", http.StatusTemporaryRedirect)
		})
	}

	return mux
}

func incomingHeaderMatcher(key string) (string, bool) {
	if strings.EqualFold(key, "Authorization") {
		return "authorization", true
	}
	return runtime.DefaultHeaderMatcher(key)
}

func outgoingHeaderMatcher(key string) (string, bool) {
	switch strings.ToLower(key) {
	case "content-disposition":
		return "Content-Disposition", true
	case "x-checksum-sha512":
		return "X-Checksum-Sha512", true
	case "x-plugin-version":
		return "X-Plugin-Version", true
	}
	return runtime.MetadataHeaderPrefix + key, true
}

type rawHTTPBodyMarshaler struct {
	*runtime.HTTPBodyMarshaler
	contentType string
}

func newRawHTTPBodyMarshaler(contentType string, fallback runtime.Marshaler) runtime.Marshaler {
	return &rawHTTPBodyMarshaler{
		HTTPBodyMarshaler: &runtime.HTTPBodyMarshaler{Marshaler: fallback},
		contentType:       contentType,
	}
}

func (m *rawHTTPBodyMarshaler) NewDecoder(reader io.Reader) runtime.Decoder {
	return &rawHTTPBodyDecoder{reader: reader, contentType: m.contentType}
}

type rawHTTPBodyDecoder struct {
	reader      io.Reader
	contentType string
}

func (d *rawHTTPBodyDecoder) Decode(value any) error {
	data, err := io.ReadAll(d.reader)
	if err != nil {
		return err
	}

	body := &httpbody.HttpBody{ContentType: d.contentType, Data: data}
	switch target := value.(type) {
	case **httpbody.HttpBody:
		*target = body
	case *httpbody.HttpBody:
		proto.Reset(target)
		proto.Merge(target, body)
	}
	return nil
}
