// Package request holds the HTTP request that tessera decides, in a form
// independent of the protocol that carried it.
package request

// Request is an HTTP request to decide. Its values are as the proxy sent
// them: Path keeps its query string.
type Request struct {
	Method string
	Host   string
	Path   string
}
