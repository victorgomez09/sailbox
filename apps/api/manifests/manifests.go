package manifests

import _ "embed"

//go:embed gateway-api-v1.yaml
var GatewayAPI string

//go:embed envoy-gateway.yaml
var EnvoyGateway string

//go:embed metal-lb.yaml
var MetalLB string
