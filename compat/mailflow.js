'use strict';
// Fetches the MailFlow 3.9.0 sources that the CardDAV checks run, unchanged, from the
// v3.9.0 tag (AGPL: they are read and executed here, never copied into this repository).
// They are cached in compat/.mailflow-cache (git-ignored) and every file is checked against
// a pinned SHA-256, so a changed or substituted file stops the harness instead of being run.
//
// The files run as they are, with their own imports: carddavClient.js needs
// hostValidation.js and safeFetch.js next to it, and fast-xml-parser and undici from npm
// (pinned in package.json to the versions of MailFlow's own lock file). Nothing is shimmed.
// The CardDAV code of MailFlow never touches its database; only carddavSync.js does, and
// that file is not run.

const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const { pathToFileURL } = require('node:url');

const TAG = 'v3.9.0';
const RAW = `https://raw.githubusercontent.com/maathimself/mailflow/${TAG}/`;
const PINS = {
    'backend/src/services/carddavClient.js': '65e164d844e470611725130b7beb642f3cc7c73ca28e9dfa075292de12e6fa88',
    'backend/src/services/safeFetch.js': '4d11719a6ec133b9ef6777be6babbb8b64850b505b7bfb4af38dda9b809f550c',
    'backend/src/services/hostValidation.js': '5c13777c186bc06b846d1cdda85facc9334e901242878c3c199eac80534b0fbe',
    'backend/src/utils/vcard.js': '6107434fc9f55c72b8bfccdc9012513405726ad6fdf5090c29cec593212de8a1',
};

const CACHE = path.join(__dirname, '.mailflow-cache', TAG);

const sha256 = (b) => crypto.createHash('sha256').update(b).digest('hex');

async function ensure(rel) {
    const file = path.join(CACHE, rel);
    const want = PINS[rel];
    if (fs.existsSync(file) && sha256(fs.readFileSync(file)) === want) return file;
    const res = await fetch(RAW + rel);
    if (!res.ok) throw new Error(`cannot download ${rel} from the ${TAG} tag: HTTP ${res.status}`);
    const body = Buffer.from(await res.arrayBuffer());
    const got = sha256(body);
    if (got !== want) throw new Error(`${rel} from the ${TAG} tag has SHA-256 ${got}, the pin is ${want}; refusing to run it`);
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, body);
    return file;
}

// Loads carddavClient.js and vcard.js of MailFlow 3.9.0.
async function loadMailflow() {
    // MailFlow's safeFetch hands an undici Agent (npm undici 6.29, MailFlow's own lock) to the
    // global fetch. MailFlow runs on Node 22, whose built-in fetch is the same undici line; a newer Node
    // has another one inside and refuses that Agent. So the global fetch is undici's own, which makes the
    // CardDAV code behave the same on whatever Node runs the harness.
    globalThis.fetch = require('undici').fetch;
    for (const rel of Object.keys(PINS)) await ensure(rel);
    // The backend of MailFlow is an ES module package; say so for the cache directory.
    fs.writeFileSync(path.join(CACHE, 'package.json'), '{"type":"module","private":true}\n');
    const client = await import(pathToFileURL(path.join(CACHE, 'backend/src/services/carddavClient.js')).href);
    const vcard = await import(pathToFileURL(path.join(CACHE, 'backend/src/utils/vcard.js')).href);
    return { ...client, parseVCard: vcard.parseVCard, generateVCard: vcard.generateVCard };
}

module.exports = { loadMailflow, TAG, PINS };
