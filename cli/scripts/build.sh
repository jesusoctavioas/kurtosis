#!/usr/bin/env bash
# 2021-07-08 WATERMARK, DO NOT REMOVE - This script was generated from the Kurtosis Bash script template

set -euo pipefail   # Bash "strict mode"
script_dirpath="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root_dirpath="$(dirname "${script_dirpath}")"



# ==================================================================================================
#                                             Constants
# ==================================================================================================
source "${script_dirpath}/_constants.sh"

DEFAULT_SHOULD_PUBLISH_ARG="false"

REPL_DOCKERFILE_GENERATOR_MODULE_DIRNAME="repl_dockerfile_generator"
REPL_DOCKERFILE_TEMPLATE_FILENAME="template.Dockerfile"

# Mapping of ReplType (as declared in the generator Go code) -> name of directory where the REPL Dockerfile template lives
declare -A REPL_DIRNAMES_TO_BUILD
REPL_DIRNAMES_TO_BUILD["javascript"]="javascript_repl_image"

REPL_OUTPUT_DOCKERFILE_SUFFIX=".Dockerfile"
REPL_DOCKERFILE_GENERATOR_GORELEASER_BUILD_ID="repl-dockerfile-generator"
REPL_DOCKERFILE_GENERATOR_BINARY_OUTPUT_FILENAME="repl-dockerfile-generator"

INTERNAL_TESTSUITE_DIRNAMES=(
    "golang_internal_testsuite"
)

# ==================================================================================================
#                                       Arg Parsing & Validation
# ==================================================================================================
show_helptext_and_exit() {
    echo "Usage: $(basename "${0}") [should_publish_arg]"
    echo ""
    echo "  should_publish_arg  Whether the build artifacts should be published (default: ${DEFAULT_SHOULD_PUBLISH_ARG})"
    echo ""
    exit 1  # Exit with an error so that if this is accidentally called by CI, the script will fail
}

should_publish_arg="${1:-"${DEFAULT_SHOULD_PUBLISH_ARG}"}"
if [ "${should_publish_arg}" != "true" ] && [ "${should_publish_arg}" != "false" ]; then
    echo "Error: Invalid should-publish arg '${should_publish_arg}'" >&2
    show_helptext_and_exit
fi

# ==================================================================================================
#                                             Main Logic
# ==================================================================================================
build_dirpath="${root_dirpath}/${BUILD_DIRNAME}"
if ! mkdir -p "${build_dirpath}"; then
    echo "Error: Couldn't create build output dir '${build_dirpath}'" >&2
    exit 1
fi

if ! docker_images_tag="$(bash "${script_dirpath}/${GET_DOCKER_IMAGES_TAG_SCRIPT_FILENAME}")"; then
    echo "Error: Couldn't get Docker images tag" >&2
    exit 1
fi

# Build REPL Dockefile-generating binary
echo "Building REPL Dockerfile-generating binary..."
repl_dockerfile_generator_binary_filepath="${root_dirpath}/${BUILD_DIRNAME}/repl-dockerfile-generator"
repl_dockerfile_generator_module_dirpath="${root_dirpath}/${REPL_DOCKERFILE_GENERATOR_MODULE_DIRNAME}"
(
    if ! cd "${repl_dockerfile_generator_module_dirpath}"; then
        echo "Error: Couldn't cd to the REPL Dockerfile-generating module dirpath '${repl_dockerfile_generator_module_dirpath}'" >&2
        exit 1
    fi
    if ! go build -o "${repl_dockerfile_generator_binary_filepath}"; then
        echo "Error: Build of the REPL Dockerfile-generating binary failed" >&2
        exit 1
    fi
)
echo "REPL Dockerfile-generating binary built successfully"

# Now, use the built binary to generate REPL Dockerfiles
echo "Generating REPL Dockerfiles..."
for repl_type in "${!REPL_DIRNAMES_TO_BUILD[@]}"; do
    repl_dirname="${REPL_DIRNAMES_TO_BUILD["${repl_type}"]}"
    repl_dockerfile_template_filepath="${root_dirpath}/${repl_dirname}/${REPL_DOCKERFILE_TEMPLATE_FILENAME}"
    if ! [ -f "${repl_dockerfile_template_filepath}" ]; then
        echo "Error: Tried to generate Dockerfile for REPL '${repl_dirname}' but no template file was found at path '${repl_dockerfile_template_filepath}'" >&2
        exit 1
    fi
    output_filepath="${build_dirpath}/${repl_dirname}${REPL_OUTPUT_DOCKERFILE_SUFFIX}"
    if ! "${repl_dockerfile_generator_binary_filepath}" "${repl_dockerfile_template_filepath}" "${output_filepath}" "${repl_type}"; then
        echo "Error: An error occurred rendering template for REPL '${repl_dirname}' at path '${repl_dockerfile_template_filepath}' to output filepath '${output_filepath}'" >&2
        exit 1
    fi
done

# Now that we have the REPL Dockerfiles, build the CLI
# vvvvvvvv Goreleaser variables vvvvvvvvvvvvvvvvvvv
export CLI_BINARY_FILENAME \
export DOCKER_IMAGES_TAG="${docker_images_tag}"
if "${should_publish_arg}"; then
    # This environment variable will be set ONLY when publishing, in the CI environment
    # See the CI config for details on how this gets set
    export FURY_TOKEN
fi
# ^^^^^^^^ Goreleaser variables ^^^^^^^^^^^^^^^^^^^

# Build a CLI binary (compatible with the current OS & arch) so that we can run interactive & testing locally via the launch-cli.sh script
cli_module_dirpath="${root_dirpath}/${CLI_MODULE_DIRNAME}"
(
    if ! cd "${cli_module_dirpath}"; then
        echo "Error: Couldn't cd to CLI module dirpath '${cli_module_dirpath}'" >&2
        exit 1
    fi
    if ! goreleaser build --rm-dist --snapshot --id "${GORELEASER_CLI_BUILD_ID}" --single-target; then
        echo "Error: Couldn't build the CLI binary for the current OS/arch" >&2
        exit 1
    fi
)

# Now that we have a CLI built from source, start the version of the engine that the CLI uses
goarch="$(go env GOARCH)"
goos="$(go env GOOS)"
cli_binary_filepath="${root_dirpath}/${GORELEASER_OUTPUT_DIRNAME}/${GORELEASER_CLI_BUILD_ID}_${goos}_${goarch}/${CLI_BINARY_FILENAME}"
if ! [ -f "${cli_binary_filepath}" ]; then
    echo "Error: Expected a CLI binary to have been built by Goreleaser at '${cli_binary_filepath}' but none exists" >&2
    exit 1
fi
if ! "${cli_binary_filepath}" engine restart; then
    echo "Error: An error occurred starting the Kurtosis engine that the CLI uses" >&2
    exit 1
fi

# Now that we have the appropriate engine version, run our internal testsuites

# We set parallelism to 4 so that we don't run too many Kurtosis tests at once
# TODO TODO FIX THIS!!!
# go test ./... -p 4



exit 99



# Use a first pass of Goreleaser to build ONLY the REPL Dockerfile-generating binary, and then generate the REPL Dockerfiles so that the second
#  pass of Goreleaser (which generates the Dockerfiles) can pick them up
if ! goreleaser build --rm-dist --snapshot --id "${REPL_DOCKERFILE_GENERATOR_BINARY_OUTPUT_FILENAME}" --single-target; then
    echo "Error: Couldn't build the REPL Dockerfile-generating binary" >&2
    exit 1
fi

# Build all the Docker images
if "${should_publish_arg}"; then
    goreleaser_release_extra_args=""
else
    goreleaser_release_extra_args="--snapshot"
fi
if ! goreleaser release --rm-dist --skip-announce ${goreleaser_release_extra_args}; then
    echo "Error: Goreleaser release of all binaries & Docker images failed" >&2
    exit 1
fi

