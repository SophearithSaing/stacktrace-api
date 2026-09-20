// Package seed embeds the explicit, credential-free development fixture.
package seed

import _ "embed"

//go:embed demo.json
var Demo []byte

//go:embed personas.json
var Personas []byte
