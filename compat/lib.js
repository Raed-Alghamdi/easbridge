'use strict';
// Support code for the compatibility harness: runs the devfake bridge as a child
// process, drives the fake Exchange through its loopback control API, and
// provides a tiny check runner. Nothing here ever contacts a real server.

const { spawn } = require('node:child_process');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const readline = require('node:readline');

const iconv = require('iconv-lite');
const libqp = require('libqp');

const USER = 'compat@example.test';

class Bridge {
    constructor() {
        this.password = 'pw-' + crypto.randomBytes(12).toString('hex');
        this.logs = [];
        this.addr = {};
    }

    async start(tlsDir, dataDir, extraEnv = {}) {
        const env = {
            ...process.env,
            EASBRIDGE_UPSTREAM: 'fake',
            EASBRIDGE_ALLOWED_USERS: USER,
            EASBRIDGE_FAKE_PASSWORD: this.password,
            EASBRIDGE_IMAP_ADDR: '127.0.0.1:0',
            EASBRIDGE_SMTP_ADDR: '127.0.0.1:0',
            EASBRIDGE_HEALTH_ADDR: '127.0.0.1:0',
            EASBRIDGE_FAKE_CONTROL_ADDR: '127.0.0.1:0',
            ...(tlsDir ? { EASBRIDGE_TLS_CERT: path.join(tlsDir, 'server.pem'), EASBRIDGE_TLS_KEY: path.join(tlsDir, 'server-key.pem') } : {}),
            EASBRIDGE_DATA_DIR: dataDir,
            EASBRIDGE_LOG_LEVEL: 'debug',
            EASBRIDGE_LOG_FORMAT: 'json',
            EASBRIDGE_PING_HEARTBEAT: '60',
            EASBRIDGE_MIME_INLINE_MAX: '4096',
            ...extraEnv,
        };
        // The contacts are on by default since 0.3 (with a fixed CardDAV port); the mail checks keep
        // them off, and the contacts checks name their own address.
        if (!('EASBRIDGE_CARDDAV_ADDR' in extraEnv) && !('EASBRIDGE_CONTACTS' in extraEnv)) env.EASBRIDGE_CONTACTS = 'off';
        this.proc = spawn(process.env.EASBRIDGE_BIN, [], { env, stdio: ['ignore', 'pipe', 'pipe'] });
        this.exited = new Promise((resolve) => this.proc.on('exit', (code, sig) => resolve({ code, sig })));
        const ready = new Promise((resolve, reject) => {
            const t = setTimeout(() => reject(new Error('bridge did not start in 15 s\n' + this.logs.join('\n'))), 15000);
            const onLine = (line) => {
                this.logs.push(line);
                let j;
                try {
                    j = JSON.parse(line);
                } catch (e) {
                    return;
                }
                if (j.msg && j.msg.includes('control API listening')) this.addr.control = j.addr;
                if (j.msg && j.msg.startsWith('contacts are on')) this.addr.carddav = j.carddav;
                if (j.msg === 'easbridge started') {
                    this.addr.imap = j.imap;
                    this.addr.smtp = j.smtp;
                    this.addr.health = j.health;
                    clearTimeout(t);
                    resolve();
                }
            };
            readline.createInterface({ input: this.proc.stderr }).on('line', onLine);
            readline.createInterface({ input: this.proc.stdout }).on('line', (l) => this.logs.push(l));
            this.proc.on('exit', (code) => {
                clearTimeout(t);
                reject(new Error(`bridge exited early (${code})\n` + this.logs.join('\n')));
            });
        });
        await ready;
        const split = (a) => {
            const i = a.lastIndexOf(':');
            return { host: a.slice(0, i), port: parseInt(a.slice(i + 1), 10) };
        };
        this.imap = split(this.addr.imap);
        this.smtp = split(this.addr.smtp);
        if (this.addr.carddav) this.carddav = split(this.addr.carddav);
    }

    async stop() {
        if (!this.proc || this.proc.exitCode !== null) return this.exited;
        this.proc.kill('SIGTERM');
        const timer = setTimeout(() => this.proc.kill('SIGKILL'), 15000);
        const res = await this.exited;
        clearTimeout(timer);
        return res;
    }

    async control(method, p, { query = {}, body } = {}) {
        const qs = new URLSearchParams({ user: USER, ...query }).toString();
        const res = await fetch(`http://${this.addr.control}${p}?${qs}`, {
            method,
            body: body === undefined ? undefined : body,
        });
        const text = await res.text();
        if (!res.ok) throw new Error(`control ${p}: HTTP ${res.status} ${text}`);
        return text ? JSON.parse(text) : null;
    }

    inject(folder, mime, opts = {}) {
        const query = { folder };
        if (opts.read) query.read = '1';
        if (opts.flagged) query.flagged = '1';
        if (opts.date) query.date = new Date(opts.date).toISOString();
        if (opts.bcc) query.bcc = opts.bcc; // the email2:Bcc property the item is listed with
        return this.control('POST', '/inject', { query, body: mime });
    }
    items(folder, withMime = false) {
        return this.control('GET', '/items', { query: { folder, mime: withMime ? '1' : '0' } });
    }
    sent() {
        return this.control('GET', '/sent');
    }
    calls(op) {
        return this.control('GET', '/calls', { query: { op } }).then((r) => r.count);
    }
    // Let `after` calls of op (Sync or FetchMIME) on a folder through, then make
    // later ones wait until release(): freezes a folder's first sync half way.
    hold(folder, op, after = 0) {
        return this.control('POST', '/hold', { query: { folder, op, after: String(after) } });
    }
    release(folder, op) {
        return this.control('POST', '/release', { query: { folder, op } });
    }
    // Contacts and the Global Address List of the fake Exchange (devfake control API).
    contactFolder(name, type = 9) {
        return this.control('POST', '/contact-folder', { query: { name, type: String(type) } });
    }
    contact(folder, fields, id) {
        const query = { folder };
        if (id) query.id = id;
        return this.control('POST', '/contact', { query, body: JSON.stringify(fields) });
    }
    contactDelete(folder, id) {
        return this.control('POST', '/contact-delete', { query: { folder, id } });
    }
    galAdd(person) {
        return this.control('POST', '/gal-add', { body: JSON.stringify(person) });
    }
    galQueries() {
        return this.control('GET', '/gal-queries');
    }
    async health() {
        const r = await fetch(`http://${this.addr.health}/healthz`);
        return r.json();
    }
}

// ---- checks ----------------------------------------------------------------

class Runner {
    constructor() {
        this.results = [];
        this.current = null;
    }
    async check(name, fn) {
        const start = Date.now();
        try {
            await fn();
            this.results.push({ name, ok: true, ms: Date.now() - start });
            console.log(`  ok    ${name} (${Date.now() - start} ms)`);
        } catch (err) {
            this.results.push({ name, ok: false, err });
            // imapflow errors say which command failed and what the server answered.
            const detail = err && (err.executedCommand || err.responseText || err.serverResponseCode)
                ? `\ncommand: ${err.executedCommand}\nresponse: ${err.responseStatus} ${err.responseText} [${err.serverResponseCode}]`
                : '';
            console.log(`  FAIL  ${name}\n        ${(String(err && err.stack ? err.stack : err) + detail).split('\n').join('\n        ')}`);
        }
    }
    summary() {
        const failed = this.results.filter((r) => !r.ok);
        console.log(`\n${this.results.length - failed.length} passed, ${failed.length} failed`);
        return failed.length === 0;
    }
}

function assert(cond, msg) {
    if (!cond) throw new Error(msg || 'assertion failed');
}
function eq(got, want, what) {
    const g = JSON.stringify(got);
    const w = JSON.stringify(want);
    if (g !== w) throw new Error(`${what || 'value'}: got ${g}, want ${w}`);
}
async function until(what, cond, { timeout = 8000, step = 25 } = {}) {
    const end = Date.now() + timeout;
    for (;;) {
        const v = await cond();
        if (v) return v;
        if (Date.now() > end) throw new Error(`timed out waiting for: ${what}`);
        await new Promise((r) => setTimeout(r, step));
    }
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ---- message builders --------------------------------------------------------

function simpleMail(id, subject, { date = new Date(), extra = '', body } = {}) {
    return Buffer.from(
        `Message-ID: <${id}>\r\nFrom: Bob Builder <bob@example.test>\r\nTo: compat@example.test\r\nSubject: ${subject}\r\n` +
            `Date: ${new Date(date).toUTCString()}\r\n${extra}MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n${body || 'body of ' + id}\r\n`
    );
}

function multipartMail(id, subject, { date = new Date(), extra = '' } = {}) {
    return Buffer.from(
        `Message-ID: <${id}>\r\nFrom: Bob Builder <bob@example.test>\r\nTo: compat@example.test, Carol <carol@example.test>\r\n` +
            `Subject: ${subject}\r\nDate: ${new Date(date).toUTCString()}\r\n${extra}MIME-Version: 1.0\r\n` +
            `Content-Type: multipart/mixed; boundary="B1"\r\n\r\n` +
            `--B1\r\nContent-Type: multipart/alternative; boundary="B2"\r\n\r\n` +
            `--B2\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nplain part of ${id}\r\n` +
            `--B2\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>html part of ${id}</p>\r\n--B2--\r\n` +
            `--B1\r\nContent-Type: application/pdf; name="report.pdf"\r\nContent-Disposition: attachment; filename="report.pdf"\r\n` +
            `Content-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK\r\n--B1--\r\n`
    );
}

// A message whose MIME nesting would exhaust go-imap's own BODYSTRUCTURE code
// (security review of v0.1.0): `levels` nested message/rfc822 parts.
function nestedRfc822(levels) {
    let s =
        'Message-ID: <hostile-nesting@attacker.test>\r\nFrom: attacker@example.test\r\nTo: compat@example.test\r\n' +
        `Subject: nested rfc822\r\nDate: ${new Date().toUTCString()}\r\n`;
    for (let i = 0; i < levels; i++) s += 'Content-Type: message/rfc822\r\n\r\nSubject: level\r\n';
    s += 'Content-Type: text/plain\r\n\r\nneedle-in-the-innermost-body\r\n';
    return Buffer.from(s);
}

// ---- Arabic mail ---------------------------------------------------------------
//
// Real-Exchange finding 3: the mailbox has items in windows-1256 (InternetCPID 1256),
// ISO-8859-6 (28596) and UTF-8 (65001). The texts are encoded here with iconv-lite, an
// implementation of its own (the bridge uses golang.org/x/text), and the checks assert what
// imapflow makes of them: the same Arabic text.

const ARABIC = {
    subject: 'تقرير الربع الأول', // First quarter report
    name: 'محمد العتيبي', // Mohammed Al-Otaibi
    body: 'مرحبا بالعالم\r\nهذا اختبار للبريد العربي\r\n', // Hello world / this is a test of Arabic mail
    long: 'اجتماع فريق المبيعات الأسبوعي لمناقشة خطة الربع القادم وتقارير الأداء والميزانية المقترحة',
};
const ARABIC_CHARSETS = ['windows-1256', 'iso-8859-6', 'utf-8'];

// An RFC 2047 encoded-word ("B" or "Q") for text in a charset.
function encodedWord(text, charset, kind) {
    const raw = iconv.encode(text, charset);
    if (kind === 'B') return `=?${charset}?B?${raw.toString('base64')}?=`;
    let q = '';
    for (const b of raw) {
        const alnum = (b >= 0x30 && b <= 0x39) || (b >= 0x41 && b <= 0x5a) || (b >= 0x61 && b <= 0x7a);
        q += alnum ? String.fromCharCode(b) : b === 0x20 ? '_' : '=' + b.toString(16).toUpperCase().padStart(2, '0');
    }
    return `=?${charset}?Q?${q}?=`;
}

// A long text as several encoded-words (none longer than 75 characters, cut between
// characters), folded onto continuation lines as mailers write them.
function longEncodedWords(text, charset, kind) {
    const words = [];
    let cur = '';
    for (const ch of text) {
        if (cur && encodedWord(cur + ch, charset, kind).length > 75) {
            words.push(encodedWord(cur, charset, kind));
            cur = ch;
        } else {
            cur += ch;
        }
    }
    if (cur) words.push(encodedWord(cur, charset, kind));
    return words.join('\r\n ');
}

// A body in a charset with a transfer encoding, as the bytes that go into the message.
function arabicBody(text, charset, cte) {
    const raw = iconv.encode(text, charset);
    if (cte === 'base64') return Buffer.from(raw.toString('base64').replace(/.{76}/g, '$&\r\n') + '\r\n');
    if (cte === 'quoted-printable') return Buffer.from(libqp.encode(raw));
    return raw;
}

// An Arabic text/plain message: the Subject and the name in From are encoded-words of the
// given kind ("B" or "Q") in the charset of the body.
function arabicMail(id, { charset, kind, cte, subject = ARABIC.subject, date = new Date() }) {
    const head =
        `Message-ID: <${id}>\r\nFrom: ${encodedWord(ARABIC.name, charset, kind === 'B' ? 'Q' : 'B')} <sender@example.test>\r\nTo: compat@example.test\r\n` +
        `Subject: ${longEncodedWords(subject, charset, kind)}\r\nDate: ${new Date(date).toUTCString()}\r\nMIME-Version: 1.0\r\n` +
        `Content-Type: text/plain; charset=${charset}\r\nContent-Transfer-Encoding: ${cte}\r\n\r\n`;
    return Buffer.concat([Buffer.from(head), arabicBody(ARABIC.body, charset, cte)]);
}

// The text of a body part fetched as bytes: transfer encoding undone, then the charset.
function decodeBodyPart(bytes, charset, cte) {
    let raw = bytes;
    if (cte === 'base64') raw = Buffer.from(bytes.toString(), 'base64');
    else if (cte === 'quoted-printable') raw = libqp.decode(bytes.toString());
    return iconv.decode(raw, charset);
}

module.exports = {
    Bridge, Runner, USER, assert, eq, until, sleep, simpleMail, multipartMail, nestedRfc822, fs, path,
    ARABIC, ARABIC_CHARSETS, encodedWord, longEncodedWords, arabicMail, decodeBodyPart,
};
