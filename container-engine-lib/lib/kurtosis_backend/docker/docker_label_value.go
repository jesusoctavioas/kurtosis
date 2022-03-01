package docker

import (
	"github.com/kurtosis-tech/stacktrace"
	"regexp"
)

const (
	dockerLabelValueRegexStr = "^.*$"

	// The maximum number of bytes that a label value can be
	// See https://github.com/docker/for-mac/issues/2208
	maxLabelValueBytes = 65518
)
var dockerLabelValueRegex = regexp.MustCompile(dockerLabelValueRegexStr)

// Represents a Docker label value that is guaranteed to be valid for the Docker engine
// NOTE: This is a struct-based enum
type DockerLabelValue struct {
	value string
}
// NOTE: This is ONLY for areas where the label value is declared statically!! Any sort of dynamic/runtime label value creation
//  should use CreateNewDockerLabelValue
func MustCreateNewDockerLabelValue(str string) *DockerLabelValue {
	key, err := CreateNewDockerLabelValue(str)
	if err != nil {
		panic(err)
	}
	return key
}
func CreateNewDockerLabelValue(str string) (*DockerLabelValue, error) {
	if !dockerLabelValueRegex.MatchString(str) {
		return nil, stacktrace.NewError("Label value string '%v' doesn't match Docker label value regex '%v'", str, dockerLabelValueRegexStr)
	}
	return &DockerLabelValue{value: str}, nil
}
func (key *DockerLabelValue) GetString() string {
	return key.value
}

