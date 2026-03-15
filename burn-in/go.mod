module github.com/pineapple/vigil-addons/burn-in

go 1.26.1

require (
	github.com/gorilla/websocket v1.5.3
	github.com/pineappledr/vigil-addons/shared/addonutil v0.0.0
	github.com/pineappledr/vigil-addons/shared/vigilclient v0.0.0
)

replace (
	github.com/pineappledr/vigil-addons/shared/addonutil => ../shared/addonutil
	github.com/pineappledr/vigil-addons/shared/vigilclient => ../shared/vigilclient
)
