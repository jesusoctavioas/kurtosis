/*
 * Copyright (c) 2022 - present Kurtosis Technologies Inc.
 * All Rights Reserved.
 */
import { Result } from "neverthrow";

//This interface was created to support development of web api file uploading and Node.js file uploading.
export interface GenericTgzArchiver {
    archiveFilesAtTemporaryDirectory(pathToArchive: string): Promise<Result<string, Error>>
}