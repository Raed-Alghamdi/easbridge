'use strict';
// The client half of the automatic-certificate check. It runs in its own Node process because
// NODE_EXTRA_CA_CERTS is read only at start: run.js starts it with that variable pointing at the
// CA that the bridge exported, and nothing else trusted (the throwaway CA of the other checks is not
// known here). imapflow, nodemailer and MailFlow 3.9.0's real CardDAV client connect with
// certificate verification on. Results go to stdout as JSON lines.

const fs = require('node:fs');
const tls = require('node:tls');
const { ImapFlow } = require('imapflow');
const nodemailer = require('nodemailer');
const { loadMailflow } = require('./mailflow');
const { mapEasbridgeHost } = require('./contacts');

const env = process.env;
const USER = env.AUTOTLS_USER;
const PASS = env.AUTOTLS_PASS;
const out = (name, ok, detail) => console.log(JSON.stringify({ name, ok, detail: detail ? String(detail) : undefined }));

async function step(name, fn) {
    try {
        await fn();
        out(name, true);
    } catch (e) {
        out(name, false, e && e.stack ? e.stack : e);
    }
}

// A TLS server with a leaf that the bridge's CA signed (by someone who has its key), and a client that connects
// to it by `name` with verification on.
function connectTo(dir, name) {
    return new Promise((resolve) => {
        const server = tls.createServer({ cert: fs.readFileSync(`${dir}/leaf.pem`), key: fs.readFileSync(`${dir}/leaf-key.pem`) }, (sock) => sock.end());
        server.listen(0, '127.0.0.1', () => {
            const sock = tls.connect({ host: '127.0.0.1', port: server.address().port, servername: name, rejectUnauthorized: true }, () => {
                sock.destroy();
                server.close();
                resolve(null);
            });
            sock.on('error', (e) => {
                server.close();
                resolve(e);
            });
        });
    });
}

(async () => {
    await step('imapflow', async () => {
        const c = new ImapFlow({
            host: env.AUTOTLS_IMAP_HOST, port: parseInt(env.AUTOTLS_IMAP_PORT, 10), secure: true, auth: { user: USER, pass: PASS },
            logger: false, tls: { rejectUnauthorized: true, servername: 'easbridge' },
        });
        await c.connect();
        const list = await c.list();
        if (!list.some((l) => l.path === 'INBOX')) throw new Error('no INBOX');
        await c.logout();
    });
    await step('nodemailer', async () => {
        const t = nodemailer.createTransport({
            host: env.AUTOTLS_SMTP_HOST, port: parseInt(env.AUTOTLS_SMTP_PORT, 10), secure: true, auth: { user: USER, pass: PASS },
            tls: { rejectUnauthorized: true, servername: 'easbridge' },
        });
        await t.verify();
        await t.sendMail({ from: USER, to: 'someone@example.test', subject: 'automatic certificate check', text: 'sent over a verified connection' });
    });
    await step('nameconstraints', async () => {
        // The CA is trusted through NODE_EXTRA_CA_CERTS, which Node applies to every connection the process makes.
        // It carries critical name constraints, so a certificate it signs for another name is refused, and one for
        // the name it was made for is accepted (so the refusal is the constraint and nothing else).
        const ok = await connectTo(env.AUTOTLS_LEAF_OK, 'easbridge');
        if (ok) throw new Error('a leaf for easbridge from the CA was refused: ' + ok.code + ' ' + ok.message);
        const bad = await connectTo(env.AUTOTLS_LEAF_BAD, 'evil.example.test');
        if (!bad) throw new Error('a leaf for evil.example.test signed by the CA was ACCEPTED');
        if (!/permitted|constraint/i.test(`${bad.code} ${bad.message}`)) throw new Error('refused for another reason than the constraints: ' + bad.code + ' ' + bad.message);
        console.error('nameconstraints: refused with', bad.code, bad.message);
    });
    await step('carddav', async () => {
        mapEasbridgeHost();
        const mf = await loadMailflow();
        const creds = { username: USER, password: PASS, allowPrivate: true };
        const books = await mf.discoverAddressBooks({ serverUrl: `https://easbridge:${env.AUTOTLS_CARDDAV_PORT}/`, ...creds });
        if (!books.some((b) => b.displayName === 'Directory')) throw new Error('no Directory book: ' + JSON.stringify(books.map((b) => b.displayName)));
        const cards = await mf.fetchAddressBookCards({ url: books[0].url, ...creds });
        if (!Array.isArray(cards)) throw new Error('no card list');
    });
})().catch((e) => {
    out('client', false, e);
    process.exitCode = 1;
});
