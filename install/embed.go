package install

import _ "embed"

//go:embed node-install.sh
var NodeScript []byte

//go:embed traefik.yml
var TraefikConfig []byte

//go:embed cli.sh
var CLIScript []byte

//go:embed cli.ps1
var CLIPowerShellScript []byte
