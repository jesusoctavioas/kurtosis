package upload_files

import (
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/kurtosis_instruction"
	"github.com/kurtosis-tech/kurtosis/core/server/commons/enclave_data_directory"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestUploadFiles_StringRepresentation(t *testing.T) {
	filePath := "github.com/kurtosis/module/lib/lib.star"
	artifactUuid, err := enclave_data_directory.NewFilesArtifactUUID()
	require.Nil(t, err)
	uploadInstruction := NewUploadFilesInstruction(
		kurtosis_instruction.NewInstructionPosition(1, 13, "dummyFile"),
		nil, nil, filePath, "dummyPathOnDisk", artifactUuid,
	)
	expectedStrRep := `upload_files(artifact_uuid="` + string(artifactUuid) + `", src_path="` + filePath + `")`
	require.Equal(t, expectedStrRep, uploadInstruction.GetCanonicalInstruction())
	require.Equal(t, expectedStrRep, uploadInstruction.String())
}
