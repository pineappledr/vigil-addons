module github.com/pineappledr/vigil-addons/zfs-manager

go 1.26.1

replace (
	github.com/pineappledr/vigil-addons/shared/addonutil => ../shared/addonutil
	github.com/pineappledr/vigil-addons/shared/vigilclient => ../shared/vigilclient
)

require (
	github.com/pineappledr/vigil-addons/shared/addonutil v0.0.0-00010101000000-000000000000
	github.com/pineappledr/vigil-addons/shared/vigilclient v0.0.0-00010101000000-000000000000
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/gorilla/websocket v1.5.3 // indirect
