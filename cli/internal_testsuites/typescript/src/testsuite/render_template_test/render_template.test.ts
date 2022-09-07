import {err, ok, Result} from "neverthrow";
import {createEnclave} from "../../test_helpers/enclave_setup";
import {checkFileContents, startFileServer} from "../../test_helpers/test_helpers";
import {TemplateAndData} from "kurtosis-core-api-lib/build/lib/enclaves/template_and_data";

const ENCLAVE_TEST_NAME         = "render-templates-test"
const IS_PARTITIONING_ENABLED   = false

const ROOT_FILE         = "config.yml"
const NESTED_FILE       = "grafana/config.yml"
const EXPECTED_CONTENTS = "Hello Stranger. The sum of [1 2 3] is 6."

jest.setTimeout(180000)

test("Test Render Templates", TestRenderTemplates)

async function TestRenderTemplates() {
    const createEnclaveResult = await createEnclave(ENCLAVE_TEST_NAME, IS_PARTITIONING_ENABLED)
    if(createEnclaveResult.isErr()) { throw createEnclaveResult.error }
    const {enclaveContext, stopEnclaveFunction} = createEnclaveResult.value
    try {
        const templateAndDataByDestRelFilepath = getTemplateAndDataByDestRelFilepath()
        const renderTemplatesResults = await enclaveContext.renderTemplates(templateAndDataByDestRelFilepath)
        if(renderTemplatesResults.isErr()) { throw renderTemplatesResults.error }

        const filesArtifactUuid = renderTemplatesResults.value

        const startFileServerResult = await startFileServer(filesArtifactUuid, ROOT_FILE, enclaveContext)
        if (startFileServerResult.isErr()){throw startFileServerResult.error}
        const {fileServerPublicIp, fileServerPublicPortNum} = startFileServerResult.value

        const testRenderedTemplatesResult = await testRenderedTemplates(templateAndDataByDestRelFilepath, fileServerPublicIp, fileServerPublicPortNum)
        if(testRenderedTemplatesResult.isErr()) { throw testRenderedTemplatesResult.error}
    } finally {
        stopEnclaveFunction()
    }
    jest.clearAllTimers()
}

//========================================================================
// Helpers
//========================================================================

// Checks rendered templates are rendered correctly and to the right files in the right subdirectories
async function testRenderedTemplates(
    templateDataByDestinationFilepath : Map<string, TemplateAndData>,
    ipAddress: string,
    portNum: number,
): Promise<Result<null, Error>> {

    for (let [renderedTemplateFilepath, _] of templateDataByDestinationFilepath) {
        let testContentResults = await checkFileContents(ipAddress, portNum, renderedTemplateFilepath, EXPECTED_CONTENTS)
        if (testContentResults.isErr()) { return  err(testContentResults.error) }
    }
    return ok(null)
}

function getTemplateAndDataByDestRelFilepath() : Map<string, TemplateAndData> {
    let templateDataByDestinationFilepath = new Map<string, TemplateAndData>()

    const template = "Hello {{.Name}}. The sum of {{.Numbers}} is {{.Answer}}."
    const templateData  = {"Name": "Stranger", "Answer": 6, "Numbers": [1, 2, 3]}
    const templateAndData = new TemplateAndData(template, templateData)

    templateDataByDestinationFilepath.set(NESTED_FILE, templateAndData)
    templateDataByDestinationFilepath.set(ROOT_FILE, templateAndData)

    return templateDataByDestinationFilepath
}
