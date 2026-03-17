module github.com/opentalon/opentalon

go 1.24.0

require (
	github.com/coder/websocket v1.8.14
	github.com/yuin/gopher-lua v1.1.1
	google.golang.org/grpc v1.69.4
	google.golang.org/protobuf v1.35.2
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.46.1 // state store (memories + sessions); pure Go, no system SQLite required
)

require github.com/google/uuid v1.6.0

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/exp v0.0.0-20251023183803-a4bb9ffd2546 // indirect
	golang.org/x/net v0.34.0 // indirect
	golang.org/x/sys v0.39.0 // indirect
	golang.org/x/text v0.32.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20241015192408-796eee8c2d53 // indirect
	modernc.org/libc v1.67.6 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
