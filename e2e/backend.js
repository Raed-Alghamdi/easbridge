'use strict';
// The stand-in for MailFlow's backend in `make e2e`: a Node 22 container that starts after the EASBridge
// container is healthy and talks to it as MailFlow would, with certificate verification on and nothing
// trusted but NODE_EXTRA_CA_CERTS=/easbridge-ca/ca.pem (the CA the bridge exported to the shared volume).
// It exits 0 only if the CA file was there at start, and IMAP, SMTP submission and CardDAV all connected
// over verified TLS. Test values only.

const fs = require('node:fs');
const https = require('node:https');
const { ImapFlow } = require('imapflow');
const nodemailer = require('nodemailer');

// The defaults are what runs in the container; the variables let the same program be rehearsed against a
// bridge on this machine (E2E_HOST=127.0.0.1, other ports, the CA file where it was exported).
const CA = process.env.E2E_CA || '/easbridge-ca/ca.pem';
const HOST = process.env.E2E_HOST || 'easbridge';
const PORTS = { imap: +(process.env.E2E_IMAP_PORT || 993), smtp: +(process.env.E2E_SMTP_PORT || 465), dav: +(process.env.E2E_DAV_PORT || 8443) };
const tlsOpts = { rejectUnauthorized: true, servername: 'easbridge' };
const user = process.env.E2E_USER;
const pass = process.env.E2E_PASS;

function fail(msg) {
    console.log('E2E FAIL ' + msg);
    process.exit(1);
}

async function main() {
    if (!fs.existsSync(CA)) fail(`${CA} was not there when the backend started: it did not wait for the bridge`);
    if (process.env.NODE_EXTRA_CA_CERTS !== CA) fail('NODE_EXTRA_CA_CERTS is ' + process.env.NODE_EXTRA_CA_CERTS);
    console.log('E2E ca.pem present at start');

    const imap = new ImapFlow({ host: HOST, port: PORTS.imap, secure: true, auth: { user, pass }, logger: false, tls: tlsOpts });
    await imap.connect();
    const boxes = await imap.list();
    if (!boxes.some((b) => b.path === 'INBOX')) fail('no INBOX in the list');
    await imap.logout();
    console.log('E2E imap ok');

    const smtp = nodemailer.createTransport({ host: HOST, port: PORTS.smtp, secure: true, auth: { user, pass }, tls: tlsOpts });
    await smtp.verify();
    await smtp.sendMail({ from: user, to: 'someone@example.test', subject: 'e2e', text: 'sent over a verified connection' });
    console.log('E2E smtp ok');

    const status = await new Promise((resolve, reject) => {
        const req = https.request({
            host: HOST, port: PORTS.dav, servername: 'easbridge', method: 'PROPFIND', path: '/', headers: { Depth: '0', Authorization: 'Basic ' + Buffer.from(`${user}:${pass}`).toString('base64') },
        }, (res) => { res.resume(); res.on('end', () => resolve(res.statusCode)); });
        req.on('error', reject);
        req.end();
    });
    if (status !== 207) fail('CardDAV answered ' + status);
    console.log('E2E carddav ok');
    console.log('E2E OK imap smtp carddav');
}

main().catch((e) => fail(String(e && e.stack ? e.stack : e)));
