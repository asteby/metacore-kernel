module github.com/asteby/metacore-kernel

go 1.23

require (
	github.com/asteby/metacore-sdk v0.0.0-local
	github.com/google/uuid v1.6.0
	github.com/tetratelabs/wazero v1.8.0
	gorm.io/gorm v1.31.1
)

require (
	github.com/Masterminds/semver/v3 v3.3.0 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	golang.org/x/text v0.20.0 // indirect
)

replace github.com/asteby/metacore-sdk => ../metacore-sdk
