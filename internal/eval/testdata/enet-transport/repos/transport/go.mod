module example.com/transport

go 1.26

require (
	github.com/cafecito-games/goenet v0.0.0
	google.golang.org/protobuf v0.0.0
)

replace github.com/cafecito-games/goenet => ./third_party/goenet
replace google.golang.org/protobuf => ./third_party/protobuf
