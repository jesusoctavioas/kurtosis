import {createEnclave} from "../../test_helpers/enclave_setup";
import {ContainerConfig, ContainerConfigBuilder, SharedPath} from "kurtosis-core-api-lib";
import {err, ok, Result} from "neverthrow";
import log from "loglevel";

const TEST_NAME = "pause-unpause-test"
const IS_PARTITIONING_ENABLED = false
const PAUSE_UNPAUSE_TEST_IMAGE =  "alpine:3.12.4"
const TEST_SERVICE_ID = "test";

jest.setTimeout(180000)

test("Test service pause", async () => {
    // ------------------------------------- ENGINE SETUP ----------------------------------------------
    const createEnclaveResult = await createEnclave(TEST_NAME, IS_PARTITIONING_ENABLED)

    if(createEnclaveResult.isErr()) { throw createEnclaveResult.error }

    const { enclaveContext, stopEnclaveFunction } = createEnclaveResult.value

    try {
        // ------------------------------------- TEST SETUP ----------------------------------------------
        const containerConfigSupplier = getContainerConfigSupplier()

        const addServiceResult = await enclaveContext.addService(TEST_SERVICE_ID, containerConfigSupplier)

        if(addServiceResult.isErr()) {
            log.error(`An error occurred starting service "${TEST_SERVICE_ID}"`);
            throw addServiceResult.error
        };

        const testServiceContext = addServiceResult.value
        await delay(5000)
        // ------------------------------------- TEST RUN ----------------------------------------------
        const pauseServiceResult = await testServiceContext.pauseService(TEST_SERVICE_ID)
        if(pauseServiceResult.isErr()){
            log.error("An error occurred pausing service.")
            throw(pauseServiceResult.error)

        }
        // Wait 5 seconds
        await delay(5000)
        const unpauseServiceResult = await testServiceContext.unpauseService(TEST_SERVICE_ID)
        if(unpauseServiceResult.isErr()){
            log.error("An error occurred unpausing service.")
            throw(unpauseServiceResult.error)

        }
        await delay(5000)

    } finally{
        stopEnclaveFunction()
    }
})

// ====================================================================================================
//                                       Private helper functions
// ====================================================================================================
function getContainerConfigSupplier(): (ipAddr:string, sharedDirectory: SharedPath) => Result<ContainerConfig, Error> {

    const containerConfigSupplier = (ipAddr:string, sharedDirectory: SharedPath): Result<ContainerConfig, Error> => {
        const entrypointArgs = ["sleep"]
        const cmdArgs = ["30"]

        const containerConfig = new ContainerConfigBuilder(PAUSE_UNPAUSE_TEST_IMAGE)
            .withEntrypointOverride(entrypointArgs)
            .withCmdOverride(cmdArgs)
            .build()

        return ok(containerConfig)
    }

    return containerConfigSupplier
}

function delay(ms: number) {
    return new Promise( resolve => setTimeout(resolve, ms) );
}