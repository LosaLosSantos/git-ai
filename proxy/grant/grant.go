// Package grant defines the authorization contract consumed by the Gitaly proxy.
package grant

import "github.com/feder-cr/open_scion/proto/go/gitalypb"

// Coordinates identify the Gitaly server authorized for a proxy connection.
type Coordinates struct {
	Address      string            `json:"address"`
	Token        string            `json:"token"`
	CallMetadata map[string]string `json:"call_metadata"`
}

// Authorization narrows a compiled proxy profile for one connection.
type Authorization struct {
	Profile              string               `json:"profile"`
	Consumer             string               `json:"consumer"`
	Repository           *gitalypb.Repository `json:"repository"`
	AllowedMethods       []string             `json:"allowed_methods"`
	ExpiresIn            int                  `json:"expires_in"`
	MaxConcurrentStreams uint32               `json:"max_concurrent_streams"`
	ProjectID            int64                `json:"project_id"`
	RootNamespaceID      int64                `json:"root_namespace_id"`
}

// Grant is the Gitaly proxy portion of the JSON response returned by Rails.
// Its field names intentionally match the existing Workhorse preauthorization
// contract.
type Grant struct {
	Coordinates   Coordinates   `json:"GitalyServer"`
	Authorization Authorization `json:"GitalyProxy"`
}
