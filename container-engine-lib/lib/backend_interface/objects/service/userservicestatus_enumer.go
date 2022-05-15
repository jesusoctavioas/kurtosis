// Code generated by "enumer -trimprefix=UserServiceStatus_ -transform=snake-upper -type=UserServiceStatus"; DO NOT EDIT.

package service

import (
	"fmt"
	"strings"
)

const _UserServiceStatusName = "REGISTEREDACTIVATEDDEACTIVATED"

var _UserServiceStatusIndex = [...]uint8{0, 10, 19, 30}

const _UserServiceStatusLowerName = "registeredactivateddeactivated"

func (i UserServiceStatus) String() string {
	if i < 0 || i >= UserServiceStatus(len(_UserServiceStatusIndex)-1) {
		return fmt.Sprintf("UserServiceStatus(%d)", i)
	}
	return _UserServiceStatusName[_UserServiceStatusIndex[i]:_UserServiceStatusIndex[i+1]]
}

// An "invalid array index" compiler error signifies that the constant values have changed.
// Re-run the stringer command to generate them again.
func _UserServiceStatusNoOp() {
	var x [1]struct{}
	_ = x[UserServiceStatus_Registered-(0)]
	_ = x[UserServiceStatus_Activated-(1)]
	_ = x[UserServiceStatus_Deactivated-(2)]
}

var _UserServiceStatusValues = []UserServiceStatus{UserServiceStatus_Registered, UserServiceStatus_Activated, UserServiceStatus_Deactivated}

var _UserServiceStatusNameToValueMap = map[string]UserServiceStatus{
	_UserServiceStatusName[0:10]:       UserServiceStatus_Registered,
	_UserServiceStatusLowerName[0:10]:  UserServiceStatus_Registered,
	_UserServiceStatusName[10:19]:      UserServiceStatus_Activated,
	_UserServiceStatusLowerName[10:19]: UserServiceStatus_Activated,
	_UserServiceStatusName[19:30]:      UserServiceStatus_Deactivated,
	_UserServiceStatusLowerName[19:30]: UserServiceStatus_Deactivated,
}

var _UserServiceStatusNames = []string{
	_UserServiceStatusName[0:10],
	_UserServiceStatusName[10:19],
	_UserServiceStatusName[19:30],
}

// UserServiceStatusString retrieves an enum value from the enum constants string name.
// Throws an error if the param is not part of the enum.
func UserServiceStatusString(s string) (UserServiceStatus, error) {
	if val, ok := _UserServiceStatusNameToValueMap[s]; ok {
		return val, nil
	}

	if val, ok := _UserServiceStatusNameToValueMap[strings.ToLower(s)]; ok {
		return val, nil
	}
	return 0, fmt.Errorf("%s does not belong to UserServiceStatus values", s)
}

// UserServiceStatusValues returns all values of the enum
func UserServiceStatusValues() []UserServiceStatus {
	return _UserServiceStatusValues
}

// UserServiceStatusStrings returns a slice of all String values of the enum
func UserServiceStatusStrings() []string {
	strs := make([]string, len(_UserServiceStatusNames))
	copy(strs, _UserServiceStatusNames)
	return strs
}

// IsAUserServiceStatus returns "true" if the value is listed in the enum definition. "false" otherwise
func (i UserServiceStatus) IsAUserServiceStatus() bool {
	for _, v := range _UserServiceStatusValues {
		if i == v {
			return true
		}
	}
	return false
}
