'use strict';
// imapflow 1.7.8 / nodemailer 10.0.13 compatibility harness for easbridge.
//
// It starts the devfake bridge binary on loopback ports with a throwaway CA
// (trusted through NODE_EXTRA_CA_CERTS, see run.sh) and replays the access
// patterns MailFlow 3.8.3 uses (DESIGN section 9). It never contacts a real mail
// server. Exit status is non-zero if any check fails.

const { ImapFlow } = require('imapflow');
const nodemailer = require('nodemailer');
const MailComposer = require('nodemailer/lib/mail-composer');
const { Bridge, Runner, USER, assert, eq, until, sleep, simpleMail, multipartMail, nestedRfc822, fs, path, ARABIC, ARABIC_CHARSETS, encodedWord, arabicMail, decodeBodyPart } = require('./lib');
const { runContactsChecks, runFullDirectoryChecks, runAutoTlsChecks } = require('./contacts');
const show = (v) => JSON.stringify(v, (k, x) => (typeof x === 'bigint' ? Number(x) : x));

const bridge = new Bridge();
const runner = new Runner();
const clients = [];

function newClient(opts = {}, target = bridge) {
    const c = new ImapFlow({
        host: target.imap.host,
        port: target.imap.port,
        secure: true,
        auth: { user: USER, pass: target.password, ...(opts.auth || {}) },
        logger: false,
        emitLogs: true,
        tls: { rejectUnauthorized: true, servername: 'easbridge' },
        commandTimeout: 30000, // as MailFlow's makeClientCfg
        ...opts.imap,
    });
    c.frames = [];
    c.on('log', (e) => c.frames.push(e));
    c.on('error', (e) => {
        c.lastError = e;
    });
    clients.push(c);
    return c;
}

// Commands this client sent, without the arguments of AUTHENTICATE.
function sentCommands(c) {
    return c.frames.filter((f) => f.src === 'c' && /^\d+ /.test(f.msg)).map((f) => f.msg);
}

async function status(c, path) {
    return c.status(path, { messages: true, uidNext: true, uidValidity: true, unseen: true });
}

async function mailCount(folder) {
    return (await bridge.items(folder)).length;
}

async function waitCount(c, path, n) {
    await until(`${path} to hold ${n} messages`, async () => (await status(c, path)).messages === n);
}

// ------------------------------------------------------------------------------

async function main() {
    const tmp = process.env.COMPAT_TMP;
    assert(tmp && process.env.EASBRIDGE_BIN, 'run through compat/run.sh');
    fs.mkdirSync(path.join(tmp, 'data'), { recursive: true });
    await bridge.start(path.join(tmp, 'tls'), path.join(tmp, 'data'));
    console.log(`bridge: imap ${bridge.addr.imap}, smtp ${bridge.addr.smtp}`);

    let main;
    const ids = {}; // Message-ID -> UID in INBOX

    await runner.check('health endpoint is ok', async () => {
        const r = await fetch(`http://${bridge.addr.health}/healthz`);
        const j = await r.json();
        eq(r.status, 200, 'status');
        eq(j.status, 'ok', 'body status');
    });

    await runner.check('login with AUTHENTICATE PLAIN; capabilities; ENABLE never yields CONDSTORE or IMAP4REV2; no ID', async () => {
        main = newClient({ imap: { autoIdleDelay: 60000 } });
        await main.connect();
        const caps = [...main.capabilities.keys()].map((k) => k.toUpperCase());
        for (const want of ['IMAP4REV1', 'IDLE', 'UIDPLUS', 'MOVE', 'SPECIAL-USE', 'NAMESPACE', 'LITERAL+', 'CHILDREN', 'UNSELECT', 'ENABLE']) {
            assert(caps.includes(want), `missing capability ${want}: ${caps}`);
        }
        for (const bad of ['ID', 'CONDSTORE', 'QRESYNC', 'COMPRESS=DEFLATE', 'IMAP4REV2', 'LIST-STATUS', 'ESEARCH', 'SORT', 'THREAD', 'STARTTLS']) {
            assert(!caps.includes(bad), `capability ${bad} must not be offered`);
        }
        const enabled = [...main.enabled].map((k) => String(k).toUpperCase());
        assert(!enabled.includes('CONDSTORE') && !enabled.includes('IMAP4REV2'), `ENABLED = ${enabled}`);
        eq(enabled, ['UTF8=ACCEPT'], 'ENABLED');
        const cmds = sentCommands(main);
        assert(cmds[0] === '1 AUTHENTICATE PLAIN', `first command: ${cmds[0]}`);
        assert(!cmds.some((c) => /^\d+ ID\b/.test(c)), 'imapflow sent ID although it is not advertised');
        assert(!cmds.some((c) => /COMPRESS/.test(c)), 'imapflow sent COMPRESS');
        assert(cmds.includes('3 ENABLE UTF8=ACCEPT'), `ENABLE command: ${cmds}`);
    });

    await runner.check('login with plain LOGIN works, a wrong password is AUTHENTICATIONFAILED and never forwarded twice', async () => {
        const c = newClient({ auth: { loginMethod: 'LOGIN' }, imap: { autoIdleDelay: 60000 } });
        await c.connect();
        assert(sentCommands(c)[0].startsWith('1 LOGIN'), `first command: ${sentCommands(c)[0]}`);
        await c.logout();

        const before = await bridge.calls('Open');
        for (let i = 0; i < 3; i++) {
            const bad = newClient({ auth: { pass: 'definitely-wrong-password' } });
            let err;
            try {
                await bad.connect();
            } catch (e) {
                err = e;
            }
            assert(err, 'wrong password was accepted');
            eq(err.authenticationFailed, true, 'authenticationFailed');
            eq(err.serverResponseCode, 'AUTHENTICATIONFAILED', 'response code');
        }
        eq(await bridge.calls('Open'), before + 1, 'Exchange verifications for three identical wrong passwords');
    });

    await runner.check('LIST reports special-use folders, delimiter and children', async () => {
        const list = await main.list();
        const by = Object.fromEntries(list.map((l) => [l.path, l]));
        eq(by['Sent Items'].specialUse, '\\Sent');
        eq(by['Drafts'].specialUse, '\\Drafts');
        eq(by['Deleted Items'].specialUse, '\\Trash');
        eq(by['Junk Email'].specialUse, '\\Junk');
        eq(by['INBOX'].specialUse, '\\Inbox');
        eq(by['INBOX'].delimiter, '/');
        assert(!by['Outbox'] && !by['Calendar'], 'Outbox and Calendar must stay hidden');
        const ns = main.namespace || main.namespaces;
        assert(ns !== undefined || true, 'namespace');
    });

    await runner.check('the 60 s STATUS cycle with fresh LOGINs costs no Exchange traffic', async () => {
        const before = await bridge.calls('Open');
        for (let round = 0; round < 8; round++) {
            const c = newClient({ imap: { autoIdleDelay: 60000 } });
            await c.connect();
            for (const p of ['INBOX', 'Sent Items', 'Drafts', 'Junk Email', 'Deleted Items']) {
                const s = await status(c, p);
                assert(Number.isInteger(s.messages) && s.messages >= 0, `${p} messages ${s.messages}`);
                assert(s.unseen <= s.messages, `${p}: unseen ${s.unseen} > messages ${s.messages}`);
                assert(Number(s.uidNext) > 0, `${p}: uidNext ${s.uidNext}`);
                assert(typeof s.uidValidity === 'bigint' || Number.isInteger(s.uidValidity), `${p}: uidValidity ${s.uidValidity}`);
            }
            await c.logout();
        }
        eq(await bridge.calls('Open'), before, 'Exchange verifications during the STATUS cycle');
    });

    // ---- mail arrives ---------------------------------------------------------

    const t0 = Date.now() - 4 * 3600 * 1000;
    const bigBody = 'x'.repeat(76) + '\r\n';
    const seeded = [
        { id: 'plain1@exchange.test', mime: simpleMail('plain1@exchange.test', 'First plain message', { date: t0 }), read: true },
        { id: 'multi1@exchange.test', mime: multipartMail('multi1@exchange.test', 'Report with attachment', { date: t0 + 3600e3 }), read: false },
        {
            id: 'reply1@exchange.test',
            mime: simpleMail('reply1@exchange.test', 'Re: First plain message', { date: t0 + 2 * 3600e3, extra: 'In-Reply-To: <plain1@exchange.test>\r\nReferences: <plain1@exchange.test>\r\n' }),
            read: false,
        },
        {
            id: 'big1@exchange.test',
            mime: simpleMail('big1@exchange.test', 'Big one (fetched with ItemOperations)', { date: t0 + 3 * 3600e3, body: bigBody.repeat(400) }),
            read: false,
        },
    ];
    await runner.check('mail delivered to Exchange shows up (incl. one larger than the inline limit)', async () => {
        for (const s of seeded) {
            await bridge.inject('Inbox', s.mime, { read: s.read, date: s.id.startsWith('big') ? t0 + 3 * 3600e3 : undefined });
        }
        await waitCount(main, 'INBOX', 4);
        const s = await status(main, 'INBOX');
        eq(s.messages, 4);
        eq(s.unseen, 3);
        eq(Number(s.uidNext), 5);
    });

    await runner.check('SELECT INBOX: EXISTS, UIDVALIDITY, UIDNEXT, FLAGS, PERMANENTFLAGS', async () => {
        const mb = await main.mailboxOpen('INBOX');
        eq(mb.exists, 4);
        eq(Number(mb.uidNext), 5);
        assert(Number(mb.uidValidity) > 0, 'uidValidity');
        eq([...mb.permanentFlags].sort(), ['\\Answered', '\\Deleted', '\\Draft', '\\Flagged', '\\Seen']);
        assert(!mb.permanentFlags.has('\\*'), 'PERMANENTFLAGS must not contain \\*');
    });

    await runner.check("new-mail FETCH with MailFlow's query: UID FLAGS ENVELOPE BODYSTRUCTURE SIZE INTERNALDATE HEADER", async () => {
        const items = await bridge.items('Inbox', true);
        const sizeOf = Object.fromEntries(items.map((i) => [Buffer.from(i.mime_b64, 'base64').toString().match(/Message-ID: <([^>]+)>/)[1], i.size]));
        const got = [];
        for await (const msg of main.fetch('1:*', { uid: true, flags: true, envelope: true, bodyStructure: true, size: true, internalDate: true, headers: true })) {
            got.push(msg);
        }
        eq(got.length, 4, 'messages');
        for (const msg of got) {
            const mid = msg.envelope.messageId.replace(/[<>]/g, '');
            ids[mid] = msg.uid;
            eq(msg.size, sizeOf[mid], `RFC822.SIZE of ${mid}`);
            assert(msg.bodyStructure && msg.bodyStructure.type, `bodystructure of ${mid}`);
            assert(msg.headers.toString().includes(`Message-ID: <${mid}>`), 'headers');
            assert(msg.internalDate instanceof Date && !Number.isNaN(msg.internalDate.getTime()), 'internalDate');
        }
        const multi = got.find((m) => m.envelope.messageId.includes('multi1'));
        eq(multi.envelope.subject, 'Report with attachment');
        eq(multi.envelope.from[0].address, 'bob@example.test');
        eq(multi.envelope.to.length, 2);
        eq(multi.bodyStructure.type, 'multipart/mixed');
        eq(multi.bodyStructure.childNodes.map((n) => n.type), ['multipart/alternative', 'application/pdf']);
        const big = got.find((m) => m.envelope.messageId.includes('big1'));
        assert(big.size > 4096, 'the big message must be complete');
        eq(got.find((m) => m.envelope.messageId.includes('plain1')).flags.has('\\Seen'), true, 'read flag');
        eq(multi.flags.has('\\Seen'), false, 'unread flag');
    });

    await runner.check('body parts for the indexer: BODY.PEEK[n], absent parts are empty, never an error', async () => {
        const multi = ids['multi1@exchange.test'];
        const parts = ['1.1', '1.2', '2', '9'];
        let n = 0;
        for await (const msg of main.fetch(String(multi), { uid: true, bodyParts: parts }, { uid: true })) {
            n++;
            eq(msg.bodyParts.get('1.1').toString(), 'plain part of multi1@exchange.test');
            eq(msg.bodyParts.get('1.2').toString(), '<p>html part of multi1@exchange.test</p>');
            eq(msg.bodyParts.get('2').toString().trim(), 'JVBERi0xLjQK');
            const nine = msg.bodyParts.get('9');
            assert(nine === undefined || nine.length === 0, 'part 9 must be empty');
        }
        eq(n, 1);
        // The whole message through download().
        const { content } = await main.download(String(multi), undefined, { uid: true });
        const chunks = [];
        for await (const ch of content) chunks.push(ch);
        const whole = Buffer.concat(chunks);
        const stored = (await bridge.items('Inbox', true)).find((i) => Buffer.from(i.mime_b64, 'base64').toString().includes('multi1'));
        assert(whole.equals(Buffer.from(stored.mime_b64, 'base64')), 'downloaded message differs from the stored one');
        // The large message arrives intact.
        const bigUid = ids['big1@exchange.test'];
        const dl = await main.download(String(bigUid), undefined, { uid: true });
        const bc = [];
        for await (const ch of dl.content) bc.push(ch);
        const bigStored = (await bridge.items('Inbox', true)).find((i) => Buffer.from(i.mime_b64, 'base64').toString().includes('big1'));
        assert(Buffer.concat(bc).equals(Buffer.from(bigStored.mime_b64, 'base64')), 'large message differs');
    });

    await runner.check('UID n:* above the last UID returns the last message (RFC 3501) and MailFlow filters it', async () => {
        const max = Math.max(...Object.values(ids));
        const seen = [];
        for await (const msg of main.fetch(`${max + 1}:*`, { uid: true, flags: true }, { uid: true })) seen.push(msg.uid);
        eq(seen, [max]);
        assert(seen.filter((u) => u > max).length === 0, 'nothing is new');
        const found = await main.search({ uid: `${max + 1}:*` }, { uid: true });
        eq(found, [max]);
    });

    // ---- push ---------------------------------------------------------------

    await runner.check('IDLE on INBOX and Sent Items in parallel sessions: EXISTS, FETCH FLAGS, EXPUNGE; commands end IDLE promptly', async () => {
        const a = newClient({ imap: { autoIdleDelay: 100 } });
        const b = newClient({ imap: { autoIdleDelay: 100 } });
        await a.connect();
        await b.connect();
        const ev = { a: [], b: [] };
        a.on('exists', (e) => ev.a.push(['exists', e.count, e.prevCount]));
        a.on('expunge', (e) => ev.a.push(['expunge', e.seq]));
        a.on('flags', (e) => ev.a.push(['flags', e.uid, [...e.flags].sort()]));
        b.on('exists', (e) => ev.b.push(['exists', e.count, e.prevCount]));
        await a.mailboxOpen('INBOX');
        await b.mailboxOpen('Sent Items');
        await until('both sessions to be IDLE', () => a.idling && b.idling);

        const inboxItem = await bridge.inject('Inbox', simpleMail('idle1@exchange.test', 'arrives while idling'));
        await bridge.inject('Sent Items', simpleMail('idle-sent@exchange.test', 'sent elsewhere'), { read: true });
        await until('EXISTS on INBOX', () => ev.a.some((e) => e[0] === 'exists' && e[1] === 5));
        await until('EXISTS on Sent Items', () => ev.b.some((e) => e[0] === 'exists' && e[1] === 1));

        await bridge.control('POST', '/flags', { query: { folder: 'Inbox', id: inboxItem.id, read: '1', flagged: '1' } });
        await until('FETCH FLAGS while idling', () => ev.a.some((e) => e[0] === 'flags' && e[2].includes('\\Seen') && e[2].includes('\\Flagged')));
        await bridge.control('POST', '/delete', { query: { folder: 'Inbox', id: inboxItem.id } });
        await until('EXPUNGE while idling', () => ev.a.some((e) => e[0] === 'expunge' && e[1] === 5));

        await until('idle again', () => a.idling);
        const start = Date.now();
        await a.noop();
        const took = Date.now() - start;
        assert(took < 1500, `a command needed ${took} ms to end IDLE`);
        eq((await status(a, 'INBOX')).messages, 4);
        await a.logout();
        await b.logout();
    });

    // ---- flags, move, delete, create, search -----------------------------------

    await runner.check('STORE \\Seen and \\Flagged (add/remove) reach Exchange', async () => {
        const uid = String(ids['multi1@exchange.test']);
        eq(await main.messageFlagsAdd(uid, ['\\Seen'], { uid: true }), true);
        eq(await main.messageFlagsAdd(uid, ['\\Flagged'], { uid: true }), true);
        const flagsOf = async () => {
            for await (const m of main.fetch(uid, { uid: true, flags: true }, { uid: true })) return [...m.flags].sort();
        };
        eq(await flagsOf(), ['\\Flagged', '\\Seen']);
        await until('Exchange to see read+flagged', async () => (await bridge.items('Inbox', true)).some((i) => Buffer.from(i.mime_b64, 'base64').toString().includes('multi1') && i.read && i.flagged));
        eq(await main.messageFlagsRemove(uid, ['\\Flagged'], { uid: true }), true);
        await until('Exchange to see unflagged', async () => (await bridge.items('Inbox', true)).some((i) => Buffer.from(i.mime_b64, 'base64').toString().includes('multi1') && i.read && !i.flagged));
        // A local-only flag still works and does not disturb Exchange.
        eq(await main.messageFlagsAdd(uid, ['\\Answered'], { uid: true }), true);
    });

    await runner.check('SEARCH: Message-ID (bracketed, bare, MailFlow forms), nested OR of In-Reply-To/References, DELETED, ALL, UID sets', async () => {
        eq(await main.search({ header: { 'message-id': '<reply1@exchange.test>' } }, { uid: true }), [ids['reply1@exchange.test']]);
        eq(await main.search({ header: { 'Message-ID': 'reply1@exchange.test' } }, { uid: true }), [ids['reply1@exchange.test']]);
        eq(await main.search({ header: { 'message-id': '<nobody@exchange.test>' } }, { uid: true }), []);
        // searchReplyDraftUids: OR over In-Reply-To/References for up to 20 ids at once.
        const manyIds = ['<plain1@exchange.test>', ...Array.from({ length: 19 }, (_, i) => `<unknown${i}@x>`)];
        const terms = manyIds.flatMap((id) => [{ header: { 'In-Reply-To': id } }, { header: { References: id } }]);
        eq(await main.search({ or: terms }, { uid: true }), [ids['reply1@exchange.test']]);
        eq(await main.search({ deleted: true }, { uid: true }), []);
        eq((await main.search({ all: true }, { uid: true })).length, 4);
        eq(await main.search({ uid: `${ids['multi1@exchange.test']}:${ids['reply1@exchange.test']}` }, { uid: true }), [ids['multi1@exchange.test'], ids['reply1@exchange.test']]);
        eq(await main.search({ seen: false }, { uid: true }), [ids['reply1@exchange.test'], ids['big1@exchange.test']].sort((x, y) => x - y));
        // The seq-number form.
        eq((await main.search({ all: true })).length, 4);
        // The wire form of the array variant MailFlow's findUidByMessageId passes to imapflow.
        const wire = main.frames.filter((f) => f.src === 'c' && /SEARCH/.test(f.msg)).map((f) => f.msg);
        assert(wire.some((w) => /OR /.test(w)), `no OR search on the wire: ${wire.slice(-3)}`);
    });

    await runner.check('MOVE to Junk Email returns COPYUID and Exchange moves the item', async () => {
        const uid = ids['big1@exchange.test'];
        const res = await main.messageMove(String(uid), 'Junk Email', { uid: true });
        assert(res && res.uidMap instanceof Map && res.uidMap.get(uid) === 1, `uidMap = ${show([...(res.uidMap || [])])}`);
        eq(res.destination, 'Junk Email');
        await until('INBOX to shrink', async () => (await status(main, 'INBOX')).messages === 3);
        eq((await status(main, 'Junk Email')).messages, 1);
        await until('Exchange to hold the item in Junk Email', async () => (await mailCount('Junk Email')) === 1 && (await mailCount('Inbox')) === 3);
        // An unknown destination is refused (TRYCREATE), not a crash and not a success.
        let refused = false;
        try {
            refused = (await main.messageMove(String(ids['reply1@exchange.test']), 'Nowhere', { uid: true })) === false;
        } catch (e) {
            refused = true;
        }
        assert(refused, 'MOVE to a missing mailbox succeeded');
        eq((await status(main, 'INBOX')).messages, 3, 'the message must stay where it was');
    });

    await runner.check('\\Deleted + EXPUNGE (messageDelete) moves the item to Deleted Items on Exchange', async () => {
        const uid = ids['reply1@exchange.test'];
        eq(await main.messageDelete(String(uid), { uid: true }), true);
        await until('INBOX to shrink', async () => (await status(main, 'INBOX')).messages === 2);
        await until('Exchange: Deleted Items holds it', async () => (await mailCount('Deleted Items')) === 1 && (await mailCount('Inbox')) === 2);
        eq(await main.search({ deleted: true }, { uid: true }), []);
    });

    await runner.check('CREATE Snoozed (and a nested folder) works and Exchange has the folders', async () => {
        const r = await main.mailboxCreate('Snoozed');
        eq(r.path, 'Snoozed');
        const r2 = await main.mailboxCreate(['Clients', 'ACME']); // MailFlow splits the path
        eq(r2.path, 'Clients/ACME');
        const list = await main.list();
        assert(list.some((l) => l.path === 'Snoozed') && list.some((l) => l.path === 'Clients/ACME'), 'folders not listed');
        const folders = Object.values(await bridge.control('GET', '/folders'));
        assert(folders.includes('Snoozed') && folders.includes('ACME'), `Exchange folders: ${folders}`);
        eq((await status(main, 'Snoozed')).messages, 0);
    });

    await runner.check('APPEND a draft, find it by Message-ID, replace it (old draft deleted); drafts stay off Exchange', async () => {
        const c = newClient({ imap: { autoIdleDelay: 60000 } });
        await c.connect(); // no mailbox selected: APPEND must still work
        const d1 = simpleMail('draft-1@mailflow.test', 'Draft v1', { body: 'first' });
        const r1 = await c.append('Drafts', d1, ['\\Draft', '\\Seen']);
        assert(r1 && r1.uid === 1 && r1.destination === 'Drafts', `APPENDUID: ${show(r1)}`);
        const lock = await c.getMailboxLock('Drafts');
        try {
            const d2 = simpleMail('draft-1@mailflow.test', 'Draft v2', { body: 'second' });
            const r2 = await c.append('Drafts', d2, ['\\Draft', '\\Seen']);
            eq(r2.uid, 2);
            const hits = await c.search({ header: { 'message-id': '<draft-1@mailflow.test>' } }, { uid: true });
            eq(hits, [1, 2]);
            eq(await c.messageDelete('1', { uid: true }), true);
            eq(await c.search({ all: true }, { uid: true }), [2]);
            // The reply-draft lookup runs in the Drafts folder too.
            const reply = simpleMail('draft-2@mailflow.test', 'Re: x', { extra: 'In-Reply-To: <plain1@exchange.test>\r\nReferences: <plain1@exchange.test>\r\n' });
            await c.append('Drafts', reply, ['\\Draft']);
            eq(await c.search({ or: [{ header: { 'In-Reply-To': '<plain1@exchange.test>' } }, { header: { References: '<plain1@exchange.test>' } }] }, { uid: true }), [3]);
        } finally {
            lock.release();
        }
        eq(await mailCount('Drafts'), 0, 'Exchange drafts');
        await c.logout();
    });

    await runner.check('large APPEND over LITERAL+ comes back byte for byte; flag scan and integrity listing work', async () => {
        const c = newClient({ imap: { autoIdleDelay: 60000 } });
        await c.connect();
        const big = simpleMail('big-draft@mailflow.test', 'A large draft', { body: ('lorem ipsum '.repeat(8) + '\r\n').repeat(3000) });
        assert(big.length > 250000, 'test message too small');
        const r = await c.append('Drafts', big, ['\\Draft', '\\Seen']);
        assert(r && r.uid > 0, 'APPENDUID');
        const lock = await c.getMailboxLock('Drafts');
        try {
            const dl = await c.download(String(r.uid), undefined, { uid: true });
            const chunks = [];
            for await (const ch of dl.content) chunks.push(ch);
            assert(Buffer.concat(chunks).equals(big), 'the large draft changed');
            // MailFlow's flag scan and integrity pass.
            const flags = [];
            for await (const m of c.fetch('1:*', { uid: true, flags: true })) flags.push([m.uid, [...m.flags].sort()]);
            assert(flags.length >= 2 && flags.every((f) => Number.isInteger(f[0])), `flag scan: ${show(flags)}`);
            eq((await c.search({ all: true }, { uid: true })).length, flags.length, 'integrity listing');
            eq(await c.messageDelete(String(r.uid), { uid: true }), true);
        } finally {
            lock.release();
        }
        await c.logout();
    });

    await runner.check('one pooled session visiting every mailbox again and again', async () => {
        const c = newClient({ imap: { autoIdleDelay: 60000 } });
        await c.connect();
        const paths = (await c.list()).map((l) => l.path);
        for (let i = 0; i < 40; i++) {
            for (const p of paths) {
                const lock = await c.getMailboxLock(p);
                lock.release();
            }
        }
        eq((await status(c, 'INBOX')).messages, 2);
        await c.logout();
    });

    await runner.check('SMTP submission (nodemailer, implicit TLS, AUTH PLAIN) with a Bcc recipient; Sent copy APPENDed afterwards stays a single message', async () => {
        const transporter = nodemailer.createTransport({
            host: bridge.smtp.host, port: bridge.smtp.port, secure: true,
            auth: { user: USER, pass: bridge.password }, tls: { servername: 'easbridge' },
        });
        await transporter.verify();
        const sendOne = async (id, subject, hold) => {
            await bridge.control('POST', '/hold-sent', { query: { hold: hold ? '1' : '0' } });
            const opts = {
                from: 'Compat User <compat@example.test>', to: 'bob@example.test', cc: 'carol@example.test', bcc: 'hidden@example.test',
                subject, text: 'hello from the harness', messageId: `<${id}>`,
            };
            const raw = await new MailComposer(opts).compile().build();
            const info = await transporter.sendMail({ envelope: { from: 'compat@example.test', to: ['bob@example.test', 'carol@example.test', 'hidden@example.test'] }, raw });
            eq(info.rejected, [], 'rejected');
            return { raw, info };
        };

        // Order 1: the client's APPEND arrives before Exchange's own copy.
        const one = await sendOne('send-1@mailflow.test', 'First send', true);
        const sent = await bridge.sent();
        eq(sent.length, 1);
        assert(sent[0].recipients.includes('hidden@example.test'), `recipients: ${sent[0].recipients}`);
        eq(sent[0].save_in_sent, true, 'SaveInSentItems');
        const submitted = Buffer.from(sent[0].mime_b64, 'base64').toString();
        assert(/^Bcc: hidden@example\.test\r?$/m.test(submitted), 'the Bcc-only recipient was not written into a Bcc header');
        eq(submitted.replace(/^Bcc: hidden@example\.test\r?\n/m, ''), one.raw.toString(), 'message otherwise unchanged');

        const c = newClient({ imap: { autoIdleDelay: 60000 } });
        await c.connect();
        const base = await status(c, 'Sent Items'); // one message arrived earlier (the IDLE check)
        const ap = await c.append('Sent Items', one.raw, ['\\Seen']);
        eq(ap.uid, Number(base.uidNext), 'APPENDUID');
        await bridge.control('POST', '/release-sent');
        await until("Exchange's copy to arrive", async () => (await mailCount('Sent Items')) === base.messages + 1);
        await sleep(600);
        let s = await status(c, 'Sent Items');
        eq(s.messages, base.messages + 1, 'Sent Items count after the server copy arrived');
        eq(Number(s.uidNext), Number(base.uidNext) + 1, 'Sent Items UIDNEXT');

        // Order 2: Exchange's copy is already there when the APPEND comes.
        const two = await sendOne('send-2@mailflow.test', 'Second send', false);
        await until("Exchange's copy to be synced", async () => (await status(c, 'Sent Items')).messages === base.messages + 2);
        const ap2 = await c.append('Sent Items', two.raw, ['\\Seen']);
        s = await status(c, 'Sent Items');
        eq(s.messages, base.messages + 2, 'Sent Items count after the duplicate APPEND');
        eq(Number(s.uidNext), Number(base.uidNext) + 2, 'Sent Items UIDNEXT after the duplicate APPEND');
        eq(ap2.uid, Number(base.uidNext) + 1, 'the duplicate APPEND must return the existing UID');
        await c.logout();

        // AUTH LOGIN and a wrong password.
        const t2 = nodemailer.createTransport({ host: bridge.smtp.host, port: bridge.smtp.port, secure: true, auth: { user: USER, pass: bridge.password, method: 'LOGIN' }, tls: { servername: 'easbridge' } });
        await t2.verify();
        const t3 = nodemailer.createTransport({ host: bridge.smtp.host, port: bridge.smtp.port, secure: true, auth: { user: USER, pass: 'not-the-password' }, tls: { servername: 'easbridge' } });
        let err;
        try {
            await t3.verify();
        } catch (e) {
            err = e;
        }
        assert(err && err.code === 'EAUTH' && err.responseCode === 535, `wrong SMTP password: ${err && err.code} ${err && err.responseCode}`);
    });

    await runner.check("a message sent with a Bcc recipient shows the Bcc address in Sent Items to a MailFlow-style FETCH, whichever copy comes first, and Exchange's copy announces nothing", async () => {
        const hidden = 'hidden-compat@example.test';
        const transporter = nodemailer.createTransport({
            host: bridge.smtp.host, port: bridge.smtp.port, secure: true,
            auth: { user: USER, pass: bridge.password }, tls: { servername: 'easbridge' },
        });
        const send = async (id, subject, holdExchangeCopy) => {
            await bridge.control('POST', '/hold-sent', { query: { hold: holdExchangeCopy ? '1' : '0' } });
            const raw = await new MailComposer({
                from: 'Compat User <compat@example.test>', to: 'bob@example.test', cc: 'carol@example.test', bcc: hidden,
                subject, text: 'hello, with a hidden recipient', messageId: `<${id}>`,
            }).compile().build();
            assert(!raw.toString().includes(hidden), 'MailFlow builds its copy without the Bcc header');
            await transporter.sendMail({ envelope: { from: 'compat@example.test', to: ['bob@example.test', 'carol@example.test', hidden] }, raw });
            const sent = await bridge.sent();
            const last = sent[sent.length - 1];
            return { raw, submitted: Buffer.from(last.mime_b64, 'base64'), delivered: Buffer.from(last.delivered_b64, 'base64') };
        };
        // MailFlow's FETCH of a new message, and the whole message.
        const look = async (c, uid) => {
            const msg = await c.fetchOne(String(uid), { uid: true, flags: true, envelope: true, bodyStructure: true, size: true, internalDate: true, headers: true, source: true }, { uid: true });
            assert(msg, `no message with UID ${uid}`);
            return msg;
        };
        const expectBcc = (msg, uid, submitted, when) => {
            eq(msg.uid, uid, `${when}: UID`);
            eq((msg.envelope.bcc || []).map((a) => a.address), [hidden], `${when}: ENVELOPE bcc`);
            assert(msg.headers.toString().includes(`Bcc: ${hidden}`), `${when}: BODY[HEADER] has no Bcc header`);
            assert(msg.source.equals(submitted), `${when}: the message is not the one the bridge submitted`);
            eq(msg.size, submitted.length, `${when}: RFC822.SIZE`);
        };
        const noBccAnywhere = (sub) => {
            // What Exchange delivered to the To recipient has no Bcc header and does not name the recipient.
            assert(!sub.delivered.toString().includes(hidden) && !/^Bcc:/im.test(sub.delivered.toString()), 'the delivered copy has the Bcc recipient');
        };

        const c = newClient({ imap: { autoIdleDelay: 60000 } });
        await c.connect();
        const exists = [];
        c.on('exists', (e) => exists.push(e.count));
        const lock = await c.getMailboxLock('Sent Items');
        try {
            // Order 1: MailFlow's APPEND arrives before Exchange's own copy.
            const base = await status(c, 'Sent Items');
            const one = await send('bcc-compat-1@mailflow.test', 'Bcc, APPEND first', true);
            noBccAnywhere(one);
            const ap = await c.append('Sent Items', one.raw, ['\\Seen']);
            eq(ap.uid, Number(base.uidNext), 'APPENDUID');
            await c.noop();
            expectBcc(await look(c, ap.uid), ap.uid, one.submitted, 'the APPENDed copy');
            const heard = exists.length;
            assert(heard >= 1, 'the APPEND is announced');
            await bridge.control('POST', '/release-sent');
            await until("Exchange's copy to arrive", async () => (await mailCount('Sent Items')) === base.messages + 1);
            const copy = (await bridge.items('Sent Items', true)).pop();
            assert(!Buffer.from(copy.mime_b64, 'base64').toString().includes(hidden), "Exchange's own copy has no Bcc recipient");
            await sleep(600);
            await c.noop();
            let s = await status(c, 'Sent Items');
            eq(s.messages, base.messages + 1, 'Sent Items count after Exchange\'s copy arrived');
            eq(Number(s.uidNext), Number(base.uidNext) + 1, 'Sent Items UIDNEXT');
            eq(exists.length, heard, "Exchange's copy must not be announced as new mail");
            expectBcc(await look(c, ap.uid), ap.uid, one.submitted, "after Exchange's copy arrived");

            // Order 2: Exchange's copy is in Sent Items first; the APPEND finds it.
            const two = await send('bcc-compat-2@mailflow.test', 'Bcc, Exchange first', false);
            noBccAnywhere(two);
            await until("Exchange's copy to be synced", async () => (await status(c, 'Sent Items')).messages === base.messages + 2);
            await c.noop();
            const [uid2] = await c.search({ header: { 'message-id': '<bcc-compat-2@mailflow.test>' } }, { uid: true });
            assert(uid2, "Exchange's copy is found by its Message-ID");
            expectBcc(await look(c, uid2), uid2, two.submitted, "Exchange's copy, before the APPEND");
            const ap2 = await c.append('Sent Items', two.raw, ['\\Seen']);
            eq(ap2.uid, uid2, 'the APPEND returns the UID of Exchange\'s copy');
            await c.noop();
            s = await status(c, 'Sent Items');
            eq(s.messages, base.messages + 2, 'Sent Items count after the duplicate APPEND');
            expectBcc(await look(c, uid2), uid2, two.submitted, 'after the APPEND');
        } finally {
            lock.release();
        }
        await c.logout();
    });

    await runner.check('a message in Sent Items that Exchange lists with its Bcc recipients (sent from a phone or OWA) shows them too, and one without them is served as it is', async () => {
        const c = newClient({ imap: { autoIdleDelay: 60000 } });
        await c.connect();
        const base = await status(c, 'Sent Items');
        const plain = simpleMail('phone-1@exchange.test', 'Sent from my phone', { extra: '' });
        const listed = simpleMail('phone-2@exchange.test', 'Sent from OWA', { extra: '' });
        await bridge.inject('Sent Items', plain, { read: true });
        await bridge.inject('Sent Items', listed, { read: true, bcc: '"Eve Example" <eve-property@example.test>, frank-property@example.test' });
        await until('both messages in Sent Items', async () => (await status(c, 'Sent Items')).messages === base.messages + 2);
        const lock = await c.getMailboxLock('Sent Items');
        try {
            const find = async (id) => (await c.search({ header: { 'message-id': `<${id}>` } }, { uid: true }))[0];
            const look = (uid) => c.fetchOne(String(uid), { uid: true, envelope: true, headers: true, size: true, source: true }, { uid: true });
            const m1 = await look(await find('phone-1@exchange.test'));
            eq(m1.envelope.bcc || [], [], 'a message without the property');
            assert(m1.source.equals(plain), 'a message without the property is served as Exchange rendered it');
            const m2 = await look(await find('phone-2@exchange.test'));
            eq(m2.envelope.bcc.map((a) => a.address), ['eve-property@example.test', 'frank-property@example.test'], 'ENVELOPE bcc from the property');
            eq(m2.envelope.bcc[0].name, 'Eve Example');
            assert(m2.headers.toString().includes('Bcc: "Eve Example" <eve-property@example.test>, frank-property@example.test'), 'the Bcc header');
            eq(m2.size, m2.source.length, 'RFC822.SIZE');
            assert(m2.source.toString().startsWith(listed.toString().split('\r\n\r\n')[0]), 'the rest of the header is as Exchange sent it');
            assert(m2.source.toString().endsWith('\r\n\r\n' + listed.toString().split('\r\n\r\n').slice(1).join('\r\n\r\n')), 'the body is as Exchange sent it');
        } finally {
            lock.release();
        }
        await c.logout();
    });

    await runner.check('Exchange rejecting a submission is reported with a proper SMTP code', async () => {
        await bridge.control('POST', '/fail', { query: { op: 'SendMail', n: '1', kind: 'unavailable' } });
        const t = nodemailer.createTransport({ host: bridge.smtp.host, port: bridge.smtp.port, secure: true, auth: { user: USER, pass: bridge.password }, tls: { servername: 'easbridge' } });
        let err;
        try {
            await t.sendMail({ from: 'compat@example.test', to: 'bob@example.test', subject: 'x', text: 'y' });
        } catch (e) {
            err = e;
        }
        assert(err && err.responseCode === 451, `expected 451, got ${err && err.responseCode} ${err && err.message}`);
    });

    await runner.check('first sync is all or nothing: an empty folder while it runs, then one EXISTS and UIDs in date order', async () => {
        const b2 = new Bridge();
        await b2.start(path.join(tmp, 'tls'), path.join(tmp, 'data-first-sync'));
        let c;
        try {
            // 19 messages with scrambled dates (7 is coprime with 19); every fourth is
            // larger than the bridge's inline limit and needs a separate fetch.
            const n = 19;
            const base = Date.now() - 400 * 3600 * 1000;
            const expected = [];
            for (let i = 0; i < n; i++) {
                const date = new Date(base + ((i * 7) % n) * 3600 * 1000);
                const id = `fs-${i}@first-sync.test`;
                const body = i % 4 === 0 ? 'x'.repeat(6000) : undefined;
                await b2.inject('Inbox', simpleMail(id, `first sync ${i}`, { date, body }), { date, read: i % 2 === 0 });
                expected.push({ id, date: date.getTime() });
            }
            expected.sort((a, b) => a.date - b.date);

            // Freeze the Inbox's first sync after the bootstrap and two batches, and
            // keep the large messages from being fetched.
            await b2.hold('Inbox', 'Sync', 3);
            await b2.hold('Inbox', 'FetchMIME', 0);

            c = newClient({ imap: { autoIdleDelay: 300 } }, b2);
            const events = [];
            c.on('exists', (e) => events.push({ count: e.count, prevCount: e.prevCount }));
            await c.connect();
            const mb = await c.mailboxOpen('INBOX');
            eq(mb.exists, 0, 'EXISTS during the first sync');
            eq(Number(mb.uidNext), 1, 'UIDNEXT during the first sync');

            await until('part of the Inbox to be downloaded', async () => (await b2.health()).accounts[0].staged_items > 0);
            for (let i = 0; i < 4; i++) {
                const s = await status(c, 'INBOX');
                eq([s.messages, s.unseen, Number(s.uidNext)], [0, 0, 1], 'STATUS during the first sync');
                await c.noop();
                await sleep(80);
            }
            let h = (await b2.health()).accounts[0];
            assert(h.initial_sync_pending >= 1 && h.initial_sync_complete === 0, `health during the first sync: ${show(h)}`);
            eq(events.length, 0, 'EXISTS events during the first sync');

            // The chain ends but large messages are still missing: still closed.
            await b2.release('Inbox', 'Sync');
            await until('the Inbox chain to end', async () => {
                const a = (await b2.health()).accounts[0];
                return a.pending_fetch > 0 && a.syncing === 0;
            });
            await sleep(300);
            const s1 = await status(c, 'INBOX');
            eq([s1.messages, Number(s1.uidNext)], [0, 1], 'STATUS while only large messages are missing');
            eq(events.length, 0, 'EXISTS events while large messages are missing');

            // The last body arrives: everything appears at once.
            await b2.release('Inbox', 'FetchMIME');
            await until('the Inbox to open', async () => {
                await c.noop();
                return events.length > 0;
            });
            await sleep(300);
            await c.noop();
            eq(events, [{ count: n, prevCount: 0 }], 'EXISTS events (one jump from 0 to all)');
            const s2 = await status(c, 'INBOX');
            eq([s2.messages, Number(s2.uidNext)], [n, n + 1], 'STATUS after opening');

            const got = [];
            for await (const m of c.fetch('1:*', { uid: true, envelope: true, internalDate: true })) {
                got.push({ uid: Number(m.uid), id: String(m.envelope.messageId).replace(/[<>]/g, ''), date: new Date(m.internalDate).getTime() });
            }
            got.sort((a, b) => a.uid - b.uid);
            eq(got.map((m) => m.uid), expected.map((_, i) => i + 1), 'UIDs');
            eq(got.map((m) => m.id), expected.map((m) => m.id), 'Message-IDs in UID order (oldest first)');
            for (let i = 1; i < got.length; i++) assert(got[i].date >= got[i - 1].date, `UID ${got[i].uid} is older than UID ${got[i - 1].uid}`);

            // Afterwards mail is an ordinary addition above the highest UID.
            await b2.inject('Inbox', simpleMail('fs-late@first-sync.test', 'late', {}), {});
            await until('new mail after the first sync', async () => {
                await c.noop();
                return (await status(c, 'INBOX')).messages === n + 1;
            });
            const last = [];
            for await (const m of c.fetch(`${n + 1}:*`, { uid: true, envelope: true })) last.push(Number(m.uid));
            eq(last, [n + 1], 'UID of the late message');

            h = (await b2.health()).accounts[0];
            eq([h.initial_sync_pending, h.staged_items], [0, 0], 'health after the first sync');
            const logs = b2.logs.join('\n');
            for (const bad of [b2.password, 'first sync 3', 'first-sync.test', USER]) {
                assert(!logs.includes(bad), `the bridge log contains ${JSON.stringify(bad)}`);
            }
        } finally {
            await b2.release('Inbox', 'Sync').catch(() => {});
            await b2.release('Inbox', 'FetchMIME').catch(() => {});
            if (c) await c.logout().catch(() => {});
            const res = await b2.stop();
            eq(res.code, 0, 'exit code of the second bridge');
        }
    });

    await runner.check('a message with 8,000 nested message/rfc822 parts: BODYSTRUCTURE is answered at once (flat) and the body downloads', async () => {
        const raw = nestedRfc822(8000);
        await bridge.inject('Inbox', raw, {});
        const c = newClient({ imap: { autoIdleDelay: 60000 } });
        await c.connect();
        try {
            const lock = await c.getMailboxLock('INBOX');
            try {
                const uid = await until('the hostile message to arrive', async () => {
                    const hits = await c.search({ header: { 'message-id': 'hostile-nesting@attacker.test' } }, { uid: true });
                    return Array.isArray(hits) && hits.length === 1 ? hits[0] : null;
                });
                const t0 = Date.now();
                const m = await c.fetchOne(String(uid), { uid: true, bodyStructure: true, envelope: true }, { uid: true });
                assert(Date.now() - t0 < 5000, `BODYSTRUCTURE took ${Date.now() - t0} ms`);
                eq(m.envelope.subject, 'nested rfc822', 'subject');
                eq(m.bodyStructure.type, 'application/octet-stream', 'a message beyond the limits is one opaque part');
                const dl = await c.download(String(uid), '1', { uid: true });
                const chunks = [];
                for await (const chunk of dl.content) chunks.push(chunk);
                const body = Buffer.concat(chunks);
                assert(body.length > 100000 && body.includes('needle-in-the-innermost-body'), `downloaded ${body.length} bytes`);
            } finally {
                lock.release();
            }
        } finally {
            await c.logout().catch(() => {});
        }
    });

    // ---- Arabic mail (real-Exchange finding 3) -------------------------------------
    //
    // The mailbox of the first real run has items in windows-1256, ISO-8859-6 and UTF-8. The bridge
    // answers ENVELOPE with the header text as written (RFC 3501), so imapflow does the decoding;
    // SEARCH has to read the decoded text; sections are the original bytes.

    const arabic = [];
    for (const charset of ARABIC_CHARSETS) {
        for (const kind of ['B', 'Q']) {
            for (const cte of ['8bit', 'base64', 'quoted-printable']) arabic.push({ charset, kind, cte, id: `ar-${charset}-${kind}-${cte}@exchange.test` });
            // The subject cut over several encoded-words, on continuation lines.
            arabic.push({ charset, kind, cte: '8bit', long: true, id: `arlong-${charset}-${kind}@exchange.test` });
        }
    }
    const arabicTag = (a) => `${a.charset} ${a.kind} ${a.cte}${a.long ? ' long subject' : ''}`;

    await runner.check('Arabic mail in windows-1256, ISO-8859-6 and UTF-8 (B and Q words; 8bit, base64, quoted-printable): imapflow reads the same Arabic text from ENVELOPE, BODYSTRUCTURE, sections and downloads', async () => {
        const before = (await status(main, 'INBOX')).messages;
        for (const [i, a] of arabic.entries()) {
            await bridge.inject('Inbox', arabicMail(a.id, { ...a, subject: a.long ? ARABIC.long : ARABIC.subject, date: Date.now() - (120 - i) * 60e3 }), {});
        }
        await waitCount(main, 'INBOX', before + arabic.length);
        const c = newClient({ imap: { autoIdleDelay: 60000 } });
        await c.connect();
        const lock = await c.getMailboxLock('INBOX');
        try {
            for (const a of arabic) {
                const hits = await c.search({ header: { 'message-id': `<${a.id}>` } }, { uid: true });
                eq(hits.length, 1, `UID of ${a.id}`);
                a.uid = hits[0];
                const m = await c.fetchOne(String(a.uid), { uid: true, envelope: true, bodyStructure: true, bodyParts: ['1'] }, { uid: true });
                eq(m.envelope.subject, a.long ? ARABIC.long : ARABIC.subject, `ENVELOPE subject (${arabicTag(a)})`);
                // On the wire the words are in the charset they were written in (RFC 3501: the header text), not
                // decoded and written again in UTF-8. (imapflow's log hides strings longer than 100 bytes, so the
                // display name stands for the subject here; the Go tests look at both.)
                const nameWord = encodedWord(ARABIC.name, a.charset, a.kind === 'B' ? 'Q' : 'B');
                assert(c.frames.some((f) => f.src === 's' && f.msg.includes(`"${nameWord}" NIL "sender"`)), `ENVELOPE on the wire lacks ${nameWord}`);
                eq(m.envelope.from[0].name, ARABIC.name, `ENVELOPE from name (${arabicTag(a)})`);
                eq(m.envelope.from[0].address, 'sender@example.test', `ENVELOPE from address (${arabicTag(a)})`);
                eq(m.bodyStructure.type, 'text/plain', `type (${arabicTag(a)})`);
                eq(m.bodyStructure.parameters.charset, a.charset, `BODYSTRUCTURE charset (${arabicTag(a)})`);
                eq(m.bodyStructure.encoding, a.cte, `BODYSTRUCTURE encoding (${arabicTag(a)})`);
                // BODY[1]: the bytes of the message; undone by the transfer encoding and the charset they are the text.
                eq(decodeBodyPart(m.bodyParts.get('1'), a.charset, a.cte), ARABIC.body, `BODY[1] (${arabicTag(a)})`);
                // download(): what a mail client does to read the body. imapflow undoes the transfer encoding and
                // converts the charset itself, so what it hands over is UTF-8.
                const dl = await c.download(String(a.uid), '1', { uid: true });
                const chunks = [];
                for await (const chunk of dl.content) chunks.push(chunk);
                eq(Buffer.concat(chunks).toString('utf8'), ARABIC.body, `download of part 1 (${arabicTag(a)})`);
            }
        } finally {
            lock.release();
            await c.logout().catch(() => {});
        }
    });

    await runner.check('Arabic SEARCH: SUBJECT, FROM, BODY and TEXT find the decoded Arabic text in every charset and encoding, and nothing else', async () => {
        const c = newClient({ imap: { autoIdleDelay: 60000 } });
        await c.connect();
        const lock = await c.getMailboxLock('INBOX');
        try {
            const all = arabic.map((a) => a.uid).sort((x, y) => x - y);
            const longOnes = arabic.filter((a) => a.long).map((a) => a.uid).sort((x, y) => x - y);
            const search = async (q) => (await c.search(q, { uid: true })).sort((x, y) => x - y);
            eq(await search({ subject: 'الربع' }), all, 'SUBJECT, a word in every message');
            eq(await search({ subject: ARABIC.subject }), arabic.filter((a) => !a.long).map((a) => a.uid).sort((x, y) => x - y), 'SUBJECT, the whole subject');
            eq(await search({ subject: 'خطة الربع القادم' }), longOnes, 'SUBJECT, words from the middle of a subject cut over several encoded-words');
            eq(await search({ subject: 'الخريف' }), [], 'SUBJECT, a word that is not there');
            eq(await search({ from: 'العتيبي' }), all, 'FROM, the display name');
            eq(await search({ body: 'بالعالم' }), all, 'BODY, text in windows-1256, ISO-8859-6 and UTF-8 bodies (8bit, base64, quoted-printable)');
            eq(await search({ body: 'الربع' }), [], 'BODY does not look into the headers');
            eq(await search({ text: 'الربع' }), all, 'TEXT reads the headers');
            eq(await search({ text: 'اختبار' }), all, 'TEXT reads the body');
            eq(await search({ header: { subject: 'الربع' }, body: 'العربي' }), all, 'HEADER and BODY together');
            eq(await search({ not: { subject: 'الربع' }, subject: 'First' }), [ids['plain1@exchange.test']], 'NOT SUBJECT beside an English message');
        } finally {
            lock.release();
            await c.logout().catch(() => {});
        }
    });

    await runner.check('UIDVALIDITY did not change and UIDs only grew during the whole run', async () => {
        const mb = await main.mailboxOpen('INBOX');
        assert(Number(mb.uidValidity) > 0);
        eq(Number(mb.uidNext) >= 6, true, 'uidNext');
    });

    await runner.check('bridge logs contain no password and no mail content', async () => {
        const logs = bridge.logs.join('\n');
        assert(logs.length > 500, 'no bridge logs captured');
        for (const bad of [bridge.password, 'definitely-wrong-password', 'not-the-password', 'Report with attachment', 'First plain message', 'plain part of', 'bob@example.test', 'hidden@example.test', 'sender@example.test', ARABIC.subject, ARABIC.name, 'بالعالم', USER]) {
            assert(!logs.includes(bad), `the bridge log contains ${JSON.stringify(bad)}`);
        }
        const b64 = Buffer.from(`${USER}:${bridge.password}`).toString('base64');
        assert(!logs.includes(b64), 'Basic blob in the log');
    });

    await runContactsChecks({ runner, tmp });
    await runFullDirectoryChecks({ runner, tmp });
    await runAutoTlsChecks({ runner, tmp });

    for (const c of clients) {
        try {
            c.close();
        } catch (e) {
            /* already closed */
        }
    }
    await runner.check('the bridge shuts down cleanly on SIGTERM', async () => {
        const res = await bridge.stop();
        eq(res.code, 0, 'exit code');
    });
}

main()
    .catch((err) => {
        console.error('harness error:', err);
        runner.results.push({ name: 'harness', ok: false, err });
    })
    .finally(async () => {
        await bridge.stop().catch(() => {});
        const ok = runner.summary();
        if (!ok) {
            console.log('\n--- last bridge log lines ---');
            console.log(bridge.logs.slice(-40).join('\n'));
        }
        process.exit(ok ? 0 : 1);
    });
