package website

import _ "embed"

// IndexHTML is the embedded Agentline landing page.
//
//go:embed index.html
var IndexHTML []byte

// InstallSH is the embedded Agentline installer.
//
//go:embed install.sh
var InstallSH []byte
