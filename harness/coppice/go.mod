module github.com/opendaisugi/coppice

go 1.26.0

toolchain go1.26.8

require (
	daisugi-verify v0.0.0
	github.com/BurntSushi/toml v1.5.0
	github.com/coder/websocket v1.8.15
	github.com/creack/pty v1.1.24
	github.com/mdp/qrterminal/v3 v3.2.1
	go.mitchellh.com/libghostty v0.0.0-20260908040635-9f448dfe8052
	golang.org/x/term v0.45.0
	modernc.org/sqlite v1.57.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.47.0 // indirect
	modernc.org/libc v1.74.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	rsc.io/qr v0.2.0 // indirect
)

replace daisugi-verify => ../../clients/go
