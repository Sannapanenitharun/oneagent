module github.com/oneagent/agent

go 1.22

require (
	gopkg.in/yaml.v3 v3.0.1
	google.golang.org/protobuf v1.34.2
	go.opentelemetry.io/proto v1.3.1
)

replace gopkg.in/yaml.v3 => ./third_party/yaml.v3
replace google.golang.org/protobuf => ./third_party/protobuf-go
replace go.opentelemetry.io/proto => ./third_party/otlp-proto
