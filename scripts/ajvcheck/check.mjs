// check.mjs compiles JSON Schemas with Ajv set up as clients built on the
// TypeScript MCP SDK set it up (createDefaultAjvInstance, in the SDK's
// validation/ajv-provider.js): Ajv's default class, which reads draft-07,
// with strict mode off, formats checked, no check of a schema against its
// meta-schema and every error collected, and the formats of ajv-formats
// added. Such a client compiles the output schema of every tool as it
// lists the tools, all on one Ajv, and catches nothing, so a schema that
// Ajv cannot compile costs it every tool of the server. CI's schemas job
// runs this over the MCP server's schema goldens, which TestSchemaGoldens
// holds to what the server serves:
//
//   node scripts/ajvcheck/check.mjs internal/mcpserver/testdata/schemas/*.json
//
// It compiles input schemas too, which other clients may compile. It names
// each file that is not JSON or does not compile, and then exits 1;
// package.json pins the versions of Ajv and ajv-formats, which npm ci
// installs from package-lock.json.

import { readFileSync } from 'node:fs';
import Ajv from 'ajv';
import addFormats from 'ajv-formats';

const files = process.argv.slice(2);
if (files.length === 0) {
  console.error('usage: node check.mjs SCHEMA.json...');
  process.exit(2);
}

const ajv = new Ajv({ strict: false, validateFormats: true, validateSchema: false, allErrors: true });
addFormats(ajv);

let failed = 0;
for (const file of files) {
  try {
    ajv.compile(JSON.parse(readFileSync(file, 'utf8')));
    console.log(`ok ${file}`);
  } catch (err) {
    failed++;
    console.error(`FAIL ${file}: ${err.message}`);
  }
}
if (failed > 0) {
  console.error(`${failed} of ${files.length} schemas do not compile`);
  process.exit(1);
}
console.log(`${files.length} schemas compile`);
