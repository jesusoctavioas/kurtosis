package service_config

import (
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/kurtosis_instruction/assert"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/kurtosis_starlark_framework"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/kurtosis_starlark_framework/builtin_argument"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/kurtosis_starlark_framework/kurtosis_type_constructor"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/recipe"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/startosis_errors"
	"go.starlark.net/starlark"
	"reflect"
	"time"
)

const (
	ReadyConditionsTypeName = "ReadyConditions"

	RecipeAttr    = "recipe"
	FieldAttr     = "field"
	AssertionAttr = "assertion"
	TargetAttr    = "target_value"
	IntervalAttr  = "interval"
	TimeoutAttr   = "timeout"

	defaultInterval = 1 * time.Second
	defaultTimeout  = 15 * time.Minute //TODO we could move these two to the service helpers method
)

func NewReadyConditionsType() *kurtosis_type_constructor.KurtosisTypeConstructor {
	return &kurtosis_type_constructor.KurtosisTypeConstructor{
		KurtosisBaseBuiltin: &kurtosis_starlark_framework.KurtosisBaseBuiltin{
			Name: ReadyConditionsTypeName,
			Arguments: []*builtin_argument.BuiltinArgument{
				{
					Name:              RecipeAttr,
					IsOptional:        false,
					ZeroValueProvider: builtin_argument.ZeroValueProvider[starlark.Value],
					Validator:         validateRecipe,
				},
				{
					Name:              FieldAttr,
					IsOptional:        false,
					ZeroValueProvider: builtin_argument.ZeroValueProvider[starlark.String],
					Validator: func(value starlark.Value) *startosis_errors.InterpretationError {
						return builtin_argument.NonEmptyString(value, FieldAttr)
					},
				},
				{
					Name:              AssertionAttr,
					IsOptional:        false,
					ZeroValueProvider: builtin_argument.ZeroValueProvider[starlark.String],
					Validator:         assert.ValidateAssertionToken,
				},
				{
					Name:              TargetAttr,
					IsOptional:        false,
					ZeroValueProvider: builtin_argument.ZeroValueProvider[starlark.Comparable],
					Validator: func(value starlark.Value) *startosis_errors.InterpretationError {
						return builtin_argument.NonEmptyString(value, FieldAttr)
					},
				},
				{
					Name:              IntervalAttr,
					IsOptional:        true,
					ZeroValueProvider: builtin_argument.ZeroValueProvider[starlark.String],
					Validator: func(value starlark.Value) *startosis_errors.InterpretationError {
						return validateDuration(value, IntervalAttr)
					},
				},
				{
					Name:              TimeoutAttr,
					IsOptional:        true,
					ZeroValueProvider: builtin_argument.ZeroValueProvider[starlark.String],
					Validator: func(value starlark.Value) *startosis_errors.InterpretationError {
						return validateDuration(value, TimeoutAttr)
					},
				},
			},
		},
		Instantiate: instantiateReadyConditions,
	}
}

func instantiateReadyConditions(arguments *builtin_argument.ArgumentValuesSet) (kurtosis_type_constructor.KurtosisValueType, *startosis_errors.InterpretationError) {
	kurtosisValueType, err := kurtosis_type_constructor.CreateKurtosisStarlarkTypeDefault(ReadyConditionsTypeName, arguments)
	if err != nil {
		return nil, err
	}
	return &ReadyConditions{
		KurtosisValueTypeDefault: kurtosisValueType,
	}, nil
}

// ReadyConditions is a starlark.Value that holds all the information needed for ensuring service readiness
type ReadyConditions struct {
	*kurtosis_type_constructor.KurtosisValueTypeDefault
}

func (readyConditions *ReadyConditions) GetRecipe() (recipe.Recipe, *startosis_errors.InterpretationError) {
	var (
		genericRecipe     recipe.Recipe
		found             bool
		httpRecipe        *recipe.HttpRequestRecipe
		execRecipe        *recipe.ExecRecipe
		interpretationErr *startosis_errors.InterpretationError
	)

	httpRecipe, found, interpretationErr = kurtosis_type_constructor.ExtractAttrValue[*recipe.HttpRequestRecipe](readyConditions.KurtosisValueTypeDefault, RecipeAttr)
	genericRecipe = httpRecipe
	if !found {
		return nil, startosis_errors.NewInterpretationError("Required attribute '%s' could not be found on type '%s'",
			RecipeAttr, ReadyConditionsTypeName)
	}
	//TODO we should rework the recipe types to inherit a single common type, this will avoid the double parsing here.
	if interpretationErr != nil {
		execRecipe, _, interpretationErr = kurtosis_type_constructor.ExtractAttrValue[*recipe.ExecRecipe](readyConditions.KurtosisValueTypeDefault, RecipeAttr)
		if interpretationErr != nil {
			return nil, interpretationErr
		}
		genericRecipe = execRecipe
	}

	return genericRecipe, nil
}

func (readyConditions *ReadyConditions) GetField() (string, *startosis_errors.InterpretationError) {
	field, found, interpretationErr := kurtosis_type_constructor.ExtractAttrValue[starlark.String](readyConditions.KurtosisValueTypeDefault, FieldAttr)
	if interpretationErr != nil {
		return "", interpretationErr
	}
	if !found {
		return "", startosis_errors.NewInterpretationError("Required attribute '%s' could not be found on type '%s'",
			FieldAttr, ReadyConditionsTypeName)
	}
	fieldStr := field.GoString()

	return fieldStr, nil
}

func (readyConditions *ReadyConditions) GetAssertion() (string, *startosis_errors.InterpretationError) {
	assertion, found, interpretationErr := kurtosis_type_constructor.ExtractAttrValue[starlark.String](readyConditions.KurtosisValueTypeDefault, AssertionAttr)
	if interpretationErr != nil {
		return "", interpretationErr
	}
	if !found {
		return "", startosis_errors.NewInterpretationError("Required attribute '%s' could not be found on type '%s'",
			AssertionAttr, ReadyConditionsTypeName)
	}
	assertionStr := assertion.GoString()

	return assertionStr, nil
}

func (readyConditions *ReadyConditions) GetTarget() (starlark.Comparable, *startosis_errors.InterpretationError) {
	target, found, interpretationErr := kurtosis_type_constructor.ExtractAttrValue[starlark.Comparable](readyConditions.KurtosisValueTypeDefault, TargetAttr)
	if interpretationErr != nil {
		return nil, interpretationErr
	}
	if !found {
		return nil, startosis_errors.NewInterpretationError("Required attribute '%s' could not be found on type '%s'",
			TargetAttr, ReadyConditionsTypeName)
	}

	return target, nil
}

func (readyConditions *ReadyConditions) GetInterval() (time.Duration, *startosis_errors.InterpretationError) {
	interval := defaultInterval

	intervalStr, found, interpretationErr := kurtosis_type_constructor.ExtractAttrValue[starlark.String](readyConditions.KurtosisValueTypeDefault, IntervalAttr)
	if interpretationErr != nil {
		return interval, interpretationErr
	}
	if found {
		parsedInterval, parseErr := time.ParseDuration(intervalStr.GoString())
		if parseErr != nil {
			return interval, startosis_errors.WrapWithInterpretationError(parseErr, "An error occurred when parsing interval '%v'", intervalStr.GoString())
		}
		interval = parsedInterval
	}

	return interval, nil
}

func (readyConditions *ReadyConditions) GetTimeout() (time.Duration, *startosis_errors.InterpretationError) {
	timeout := defaultTimeout

	timeoutStr, found, interpretationErr := kurtosis_type_constructor.ExtractAttrValue[starlark.String](readyConditions.KurtosisValueTypeDefault, TimeoutAttr)
	if interpretationErr != nil {
		return timeout, interpretationErr
	}
	if found {
		parsedTimeout, parseErr := time.ParseDuration(timeoutStr.GoString())
		if parseErr != nil {
			return timeout, startosis_errors.WrapWithInterpretationError(parseErr, "An error occurred when parsing timeout '%v'", timeoutStr.GoString())
		}
		timeout = parsedTimeout
	}

	return timeout, nil
}

func validateRecipe(value starlark.Value) *startosis_errors.InterpretationError {
	_, ok := value.(*recipe.HttpRequestRecipe)
	if !ok {
		//TODO we should rework the recipe types to inherit a single common type, this will avoid the double parsing here.
		_, ok := value.(*recipe.ExecRecipe)
		if !ok {
			return startosis_errors.NewInterpretationError("The '%s' attribute is not a Recipe (was '%s').", RecipeAttr, reflect.TypeOf(value))
		}
	}
	return nil
}

func validateDuration(value starlark.Value, attributeName string) *startosis_errors.InterpretationError {
	valueStarlarkStr, ok := value.(starlark.String)
	if !ok {
		return startosis_errors.NewInterpretationError("The '%s' attribute is not a valid string type (was '%s').", attributeName, reflect.TypeOf(value))
	}

	if valueStarlarkStr.GoString() == "" {
		return nil
	}

	_, parseErr := time.ParseDuration(valueStarlarkStr.GoString())
	if parseErr != nil {
		return startosis_errors.WrapWithInterpretationError(parseErr, "The value '%v' of '%s' attribute is not a valid duration string format", valueStarlarkStr.GoString(), attributeName)
	}

	return nil
}
