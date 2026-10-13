package httpx

import "net/http"

// Mux is the part of *http.ServeMux that modules need to register routes.
type Mux interface {
	Handle(pattern string, handler http.Handler)
}

type routeRecorder interface {
	Record(path string, methods []string)
}

// Route registers path for the given methods only (anything else gets the standard 405) and,
// when the mux can record routes, reports it. The recorded list is what the OpenAPI
// conformance test compares with docs/openapi.yaml.
func Route(mux Mux, path string, h http.Handler, methods ...string) {
	if rr, ok := mux.(routeRecorder); ok {
		rr.Record(path, methods)
	}
	mux.Handle(path, AllowMethods(h, methods...))
}
