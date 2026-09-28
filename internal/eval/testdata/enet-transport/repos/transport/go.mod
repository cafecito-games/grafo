module example.com/transport

go 1.26

require (
	github.com/codecat/go-enet v0.0.0
	google.golang.org/protobuf v0.0.0
)

replace github.com/codecat/go-enet => ./third_party/enet
replace google.golang.org/protobuf => ./third_party/protobuf
