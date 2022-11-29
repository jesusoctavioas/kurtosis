import {createEnclave} from "../../test_helpers/enclave_setup";
import {
    DEFAULT_DRY_RUN,
    IS_PARTITIONING_ENABLED,
    JEST_TIMEOUT_MS,
} from "./shared_constants";
import * as path from "path";
import log from "loglevel";
import {readStreamContentUntilClosed} from "../../test_helpers/startosis_helpers";

const VALID_MODULE_WITH_MODULE_INPUT_TEST_NAME = "valid-module-with-input"
const VALID_MODULE_WITH_MODULE_INPUT_REL_PATH = "../../../../startosis/valid-kurtosis-module-with-input"

jest.setTimeout(JEST_TIMEOUT_MS)

test("Test valid startosis module with input", async () => {
    // ------------------------------------- ENGINE SETUP ----------------------------------------------
    const createEnclaveResult = await createEnclave(VALID_MODULE_WITH_MODULE_INPUT_TEST_NAME, IS_PARTITIONING_ENABLED)

    if (createEnclaveResult.isErr()) {
        throw createEnclaveResult.error
    }

    const {enclaveContext, stopEnclaveFunction} = createEnclaveResult.value

    try {
        // ------------------------------------- TEST SETUP ----------------------------------------------
        const moduleRootPath = path.join(__dirname, VALID_MODULE_WITH_MODULE_INPUT_REL_PATH)

        log.info(`Loading module at path '${moduleRootPath}'`)

        const params = `{"greetings": "bonjour!"}`
        const outputStream = await enclaveContext.runStarlarkPackage(moduleRootPath, params, DEFAULT_DRY_RUN)

        if (outputStream.isErr()) {
            log.error(`An error occurred execute startosis module '${moduleRootPath}'`);
            throw outputStream.error
        }
        const [scriptOutput, _, interpretationError, validationErrors, executionError] = await readStreamContentUntilClosed(outputStream.value);

        expect(interpretationError).toBeUndefined()
        expect(validationErrors).toEqual([])
        expect(executionError).toBeUndefined()

        const expectedScriptOutput = "bonjour!\nHello World!\n"

        expect(scriptOutput).toEqual(expectedScriptOutput)
    } finally {
        stopEnclaveFunction()
    }
})

test("Test valid startosis module with input - missing key in params", async () => {
    // ------------------------------------- ENGINE SETUP ----------------------------------------------
    const createEnclaveResult = await createEnclave(VALID_MODULE_WITH_MODULE_INPUT_TEST_NAME, IS_PARTITIONING_ENABLED)

    if (createEnclaveResult.isErr()) {
        throw createEnclaveResult.error
    }

    const {enclaveContext, stopEnclaveFunction} = createEnclaveResult.value

    try {
        // ------------------------------------- TEST SETUP ----------------------------------------------
        const moduleRootPath = path.join(__dirname, VALID_MODULE_WITH_MODULE_INPUT_REL_PATH)

        log.info(`Loading module at path '${moduleRootPath}'`)

        const params = `{"hello": "world"}` // expecting key 'greetings' here
        const outputStream = await enclaveContext.runStarlarkPackage(moduleRootPath, params, DEFAULT_DRY_RUN)

        if (outputStream.isErr()) {
            log.error(`An error occurred execute startosis module '${moduleRootPath}'`);
            throw outputStream.error
        }
        const [scriptOutput, _, interpretationError, validationErrors, executionError] = await readStreamContentUntilClosed(outputStream.value);

        expect(interpretationError).not.toBeUndefined()
        expect(interpretationError?.getErrorMessage()).toContain("Evaluation error: struct has no .greetings attribute")
        expect(validationErrors).toEqual([])
        expect(executionError).toBeUndefined()

        expect(scriptOutput).toEqual("")
    } finally {
        stopEnclaveFunction()
    }
})
