'use strict';
// CardDAV checks: MailFlow 3.9.0's real carddavClient.js and vcard.js (fetched unchanged from
// the v3.9.0 tag, see mailflow.js) against a devfake bridge with the contacts feature on and
// a throwaway CA. The host the client uses is `easbridge`, as in MailFlow's network. Nothing
// here talks to a real server.

const dns = require('node:dns');
const https = require('node:https');
const { Bridge, USER, assert, eq, until, sleep, simpleMail, fs, path } = require('./lib');
const { loadMailflow } = require('./mailflow');

// `easbridge` is a Docker network alias in production; here it is the loopback address. The
// client's guarded connector resolves names with dns.lookup, which is replaced for that name.
function mapEasbridgeHost() {
    const orig = dns.lookup;
    dns.lookup = function (host, options, cb) {
        if (typeof options === 'function') {
            cb = options;
            options = {};
        }
        if (host === 'easbridge') {
            if (options && options.all) return process.nextTick(cb, null, [{ address: '127.0.0.1', family: 4 }]);
            return process.nextTick(cb, null, '127.0.0.1', 4);
        }
        return orig.call(dns, host, options, cb);
    };
}

// A request to the bridge with a path and a method the MailFlow client never sends.
function raw(bridge, method, p, { body, headers = {}, user = USER, pass = bridge.password } = {}) {
    return new Promise((resolve, reject) => {
        const req = https.request(
            {
                host: '127.0.0.1', port: bridge.carddav.port, method, path: p, servername: 'easbridge',
                headers: {
                    Authorization: 'Basic ' + Buffer.from(`${user}:${pass}`).toString('base64'),
                    // Node sends no length for a DELETE or a GET and would then send the body unframed.
                    ...(body === undefined ? {} : { 'Content-Length': Buffer.byteLength(body) }),
                    ...headers,
                },
            },
            (res) => {
                const chunks = [];
                res.on('data', (c) => chunks.push(c));
                res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks).toString('utf8') }));
            }
        );
        req.on('error', reject);
        req.end(body);
    });
}

const propfindCtag = `<?xml version="1.0" encoding="utf-8"?><propfind xmlns="DAV:" xmlns:cs="http://calendarserver.org/ns/"><prop><cs:getctag/></prop></propfind>`;
async function ctagOf(bridge, bookPath) {
    const r = await raw(bridge, 'PROPFIND', bookPath, { body: propfindCtag, headers: { Depth: '0', 'Content-Type': 'application/xml' } });
    assert(r.status === 207, `PROPFIND ctag: ${r.status}`);
    const m = /<CS:getctag>([^<]*)<\/CS:getctag>/.exec(r.body);
    assert(m, 'no getctag in ' + r.body);
    return m[1];
}

// A TLS proxy in front of the bridge that answers 403 to addressbook-query, the way Google's
// server does, so that MailFlow's client takes its second path: a PROPFIND listing and
// addressbook-multiget REPORTs in batches. It records what the client sent.
function startRefusingProxy(bridge, tlsDir) {
    const seen = [];
    const server = https.createServer(
        { key: fs.readFileSync(path.join(tlsDir, 'server-key.pem')), cert: fs.readFileSync(path.join(tlsDir, 'server.pem')) },
        (req, res) => {
            const chunks = [];
            req.on('data', (c) => chunks.push(c));
            req.on('end', () => {
                const body = Buffer.concat(chunks).toString('utf8');
                seen.push({ method: req.method, depth: req.headers.depth, body });
                if (req.method === 'REPORT' && body.includes('addressbook-query')) {
                    res.writeHead(403);
                    return res.end('refused');
                }
                const up = https.request(
                    { host: '127.0.0.1', port: bridge.carddav.port, method: req.method, path: req.url, servername: 'easbridge', headers: req.headers },
                    (ur) => {
                        res.writeHead(ur.statusCode, ur.headers);
                        ur.pipe(res);
                    }
                );
                up.on('error', () => res.destroy());
                up.end(Buffer.concat(chunks));
            });
        }
    );
    return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve({ server, port: server.address().port, seen })));
}

async function runContactsChecks({ runner, tmp }) {
    mapEasbridgeHost();
    const mf = await loadMailflow();
    const bridge = new Bridge();
    const dataDir = path.join(tmp, 'data-contacts');
    fs.mkdirSync(dataDir, { recursive: true });
    await bridge.start(path.join(tmp, 'tls'), dataDir, {
        EASBRIDGE_CARDDAV_ADDR: '127.0.0.1:0',
        EASBRIDGE_DIRECTORY_DOMAINS: 'corp.example',
        EASBRIDGE_FAKE_CONTACTS_INTERVAL: '250ms',
        EASBRIDGE_FAKE_SEARCH_SPACING: '100ms',
    });
    assert(bridge.carddav, 'the bridge did not report a CardDAV address');
    console.log(`contacts bridge: carddav ${bridge.addr.carddav}`);
    const creds = () => ({ username: USER, password: bridge.password, allowPrivate: true });
    const root = () => `https://easbridge:${bridge.carddav.port}/`;

    // ---- content of the fake Exchange -------------------------------------------------
    const ARABIC = { first: 'عبدالرحمن', last: 'السلمي', company: 'شركة الأمثلة المحدودة', long: 'مدير إدارة الأنظمة والتطبيقات والبنية التحتية لتقنية المعلومات والاتصالات' };
    await bridge.contactFolder('Contacts', 9);
    await bridge.contactFolder('Clients', 14);
    await bridge.contactFolder('Bulk', 14);
    const ada = (await bridge.contact('Contacts', { FirstName: 'Ada', LastName: 'Lovelace', Email1: 'ada@example.test', Email2: 'ada.home@example.test', BusinessPhone: '+1 555 011 0100', MobilePhone: '+1 555 050 0101', HomePhone: '+1 555 011 0102', Company: 'Analytical Engines, Inc.', Department: 'R&D', JobTitle: 'Countess', Office: 'Tower B' })).id;
    const bob = (await bridge.contact('Contacts', { FirstName: 'Bob', LastName: 'Builder', Email1: 'bob@example.test' })).id;
    const arabic = (await bridge.contact('Contacts', { FirstName: ARABIC.first, LastName: ARABIC.last, Company: ARABIC.company, JobTitle: ARABIC.long, Email1: 'abdulrahman@example.test', MobilePhone: '+1 555 050 4567', Office: 'برج ب' })).id;
    await bridge.contact('Clients', { FirstName: 'Cy', LastName: 'Client', Email1: 'cy@example.test' });
    for (let i = 0; i < 130; i++) await bridge.contact('Bulk', { FirstName: `Bulk${String(i).padStart(3, '0')}`, LastName: 'Person', Email1: `bulk${i}@example.test` });
    await bridge.galAdd({ DisplayName: 'Pat Smith', FirstName: 'Pat', LastName: 'Smith', EmailAddress: 'pat@corp.example', Title: 'Engineer', Company: 'Example Co', Office: 'B2', Phone: '+1 555 011 7777', MobilePhone: '+1 555 050 7778', Alias: 'psmith', HomePhone: '+1 555 055 0000' });
    await bridge.galAdd({ DisplayName: 'Patricia Jones', FirstName: 'Patricia', LastName: 'Jones', EmailAddress: 'patricia@corp.example' });
    await bridge.inject('Inbox', Buffer.from(
        `Message-ID: <dir1@x>\r\nFrom: Bob Builder <bob@example.test>\r\nTo: compat@example.test\r\nCc: Pat Smith <pat@corp.example>\r\nSubject: s\r\nDate: ${new Date().toUTCString()}\r\n\r\nhello\r\n`
    ));

    let books = [];
    await runner.check("discovery as MailFlow 3.9.0's client does it: the bare URL, the DAV root, a wrong path (well-known redirect) and the principal each find every address book", async () => {
        let last;
        books = await until('the address books (last answer: ' + 'see below' + ')', async () => {
            try {
                const b = await mf.discoverAddressBooks({ serverUrl: root(), ...creds() });
                last = `${b.length} books`;
                return b.length === 4 ? b : null;
            } catch (e) {
                last = e.message + (e.cause ? ' / ' + e.cause.message + ' ' + (e.cause.code || '') : '');
                return null; // before the first folder sync there are no books yet
            }
        }, { timeout: 8000 }).catch((e) => {
            throw new Error(`${e.message}; last result: ${last}`);
        });
        eq(books.map((b) => b.displayName).sort(), ['Bulk', 'Clients', 'Directory', 'Exchange contacts'], 'address book names');
        for (const url of [`${root()}dav/`, `${root()}nothing-here`, `${root()}dav/principals/me/`]) {
            const again = await mf.discoverAddressBooks({ serverUrl: url, ...creds() });
            eq(again.map((b) => b.url).sort(), books.map((b) => b.url).sort(), `books found from ${url}`);
        }
        for (const b of books) assert(b.url.startsWith(`https://easbridge:${bridge.carddav.port}/dav/addressbooks/me/`), 'book url ' + b.url);
    });

    const book = (name) => books.find((b) => b.displayName === name);
    const cardsOf = async (name) => (await mf.fetchAddressBookCards({ url: book(name).url, ...creds() })).map((c) => ({ ...c, parsed: mf.parseVCard(c.vcard) }));

    await runner.check("addressbook-query (MailFlow's first listing path): each contact arrives with an etag and a vCard that MailFlow's own parser reads into name, e-mail, phones by type and organization", async () => {
        const cards = await until('the three contacts', async () => {
            const c = await cardsOf('Exchange contacts');
            return c.length === 3 ? c : null;
        });
        const a = cards.find((c) => c.parsed.firstName === 'Ada');
        assert(a, 'no Ada');
        eq(a.parsed.lastName, 'Lovelace');
        eq(a.parsed.displayName, 'Ada Lovelace');
        assert(/^eas-[0-9a-f]{32}$/.test(a.parsed.uid), 'uid ' + a.parsed.uid);
        eq(a.parsed.emails.map((e) => e.value), ['ada@example.test', 'ada.home@example.test']);
        eq(a.parsed.emails[0].primary, true);
        eq(a.parsed.phones.map((p) => [p.value, p.type]).sort(), [['+1 555 011 0100', 'work'], ['+1 555 011 0102', 'home'], ['+1 555 050 0101', 'cell']]);
        eq(a.parsed.organization, 'Analytical Engines, Inc.');
        assert(/^"[0-9a-f]{64}"$/.test(`"${a.etag}"`) && a.etag.length === 64, 'etag ' + a.etag);
        assert(a.vcard.includes('TITLE:Countess') && a.vcard.includes('ORG:Analytical Engines\\, Inc.;R&D') && a.vcard.includes('NOTE:Office: Tower B'), 'the card carries title, department and office:\n' + a.vcard);
        assert(a.href.startsWith(book('Exchange contacts').url) && a.href.endsWith(`${a.parsed.uid}.vcf`), 'href ' + a.href);
        // The \r\n line ends survived XML (they arrive as &#13;) and every property is on its own line.
        assert(a.vcard.includes('\r\n') && a.vcard.startsWith('BEGIN:VCARD') && a.vcard.endsWith('END:VCARD'), 'line ends');
    });

    await runner.check('Arabic names, an Arabic company and a long Arabic job title (folded at 75 octets) are read back unchanged by MailFlow\'s parser', async () => {
        const cards = await cardsOf('Exchange contacts');
        const c = cards.find((x) => x.parsed.emails[0] && x.parsed.emails[0].value === 'abdulrahman@example.test');
        assert(c, 'the Arabic contact is missing');
        eq(c.parsed.firstName, ARABIC.first);
        eq(c.parsed.lastName, ARABIC.last);
        eq(c.parsed.organization, ARABIC.company);
        eq(c.parsed.displayName, `${ARABIC.first} ${ARABIC.last}`);
        eq(c.parsed.phones.map((p) => p.type), ['cell']);
        const lines = c.vcard.split(/\r?\n/);
        assert(lines.some((l) => l.startsWith(' ')), 'the long title should have been folded');
        // Folded lines have at most 75 octets and no UTF-8 sequence is cut.
        const wire = await raw(bridge, 'GET', new URL(c.href).pathname);
        assert(wire.status === 200 && wire.headers['content-type'] === 'text/vcard; charset=utf-8', 'GET: ' + wire.status);
        for (const line of wire.body.split('\r\n')) assert(Buffer.byteLength(line) <= 75, `a line of ${Buffer.byteLength(line)} octets`);
        assert(!wire.body.includes('�'), 'a replacement character: an octet sequence was cut');
        assert(wire.body.replace(/\r\n /g, '').includes(ARABIC.long), 'the long title is whole once unfolded');
    });

    await runner.check('the ctag of a book changes when a contact is edited in Exchange, and the new card has another etag', async () => {
        const bookPath = new URL(book('Exchange contacts').url).pathname;
        const before = await ctagOf(bridge, bookPath);
        const oldCards = await cardsOf('Exchange contacts');
        const oldEtag = oldCards.find((c) => c.parsed.firstName === 'Bob').etag;
        await bridge.contact('Contacts', { FirstName: 'Bob', LastName: 'Builder-Edited', Email1: 'bob@example.test' }, bob);
        await until('a new ctag', async () => (await ctagOf(bridge, bookPath)) !== before);
        const after = await cardsOf('Exchange contacts');
        const b = after.find((c) => c.parsed.firstName === 'Bob');
        eq(b.parsed.lastName, 'Builder-Edited');
        assert(b.etag !== oldEtag, 'the etag did not change');
        eq(b.parsed.uid, oldCards.find((c) => c.parsed.firstName === 'Bob').parsed.uid, 'the UID is stable');
        // A quiet period: the ctag does not move without a change.
        const quiet = await ctagOf(bridge, bookPath);
        await sleep(900);
        eq(await ctagOf(bridge, bookPath), quiet, 'ctag without a change');
    });

    await runner.check('a contact deleted in Exchange disappears from the listing, from a multiget and moves the ctag', async () => {
        const bookPath = new URL(book('Exchange contacts').url).pathname;
        const cards = await cardsOf('Exchange contacts');
        const victim = cards.find((c) => c.parsed.firstName === 'Bob');
        const before = await ctagOf(bridge, bookPath);
        await bridge.contactDelete('Contacts', bob);
        await until('the card is gone', async () => (await cardsOf('Exchange contacts')).length === 2);
        assert((await ctagOf(bridge, bookPath)) !== before, 'ctag');
        const r = await raw(bridge, 'REPORT', bookPath, {
            headers: { 'Content-Type': 'application/xml' },
            body: `<?xml version="1.0"?><C:addressbook-multiget xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><prop><getetag/><C:address-data/></prop><href>${new URL(victim.href).pathname}</href></C:addressbook-multiget>`,
        });
        assert(r.status === 207 && r.body.includes('404 Not Found') && !r.body.includes('Builder'), 'multiget of the deleted card: ' + r.body);
        assert((await raw(bridge, 'GET', new URL(victim.href).pathname)).status === 404, 'GET of the deleted card');
    });

    await runner.check("the second listing path: when addressbook-query is refused (403), the client lists with PROPFIND and fetches addressbook-multiget in batches of 100 without a Depth header", async () => {
        const proxy = await startRefusingProxy(bridge, path.join(tmp, 'tls'));
        try {
            const proxied = book('Bulk').url.replace(`:${bridge.carddav.port}/`, `:${proxy.port}/`);
            const cards = await until('130 contacts in Bulk', async () => {
                proxy.seen.length = 0;
                const c = await mf.fetchAddressBookCards({ url: proxied, ...creds() });
                return c.length === 130 ? c : null;
            }, { timeout: 20000 });
            eq(new Set(cards.map((c) => mf.parseVCard(c.vcard).uid)).size, 130, 'distinct cards');
            const kinds = proxy.seen.map((s) => `${s.method}${s.depth ? ' depth=' + s.depth : ''}${s.body.includes('addressbook-query') ? ' query' : s.body.includes('addressbook-multiget') ? ' multiget' : ''}`);
            eq(kinds, ['REPORT depth=1 query', 'PROPFIND depth=1', 'REPORT multiget', 'REPORT multiget'], 'the requests of the second path');
            eq(proxy.seen.slice(2).map((s) => (s.body.match(/<href>/g) || []).length), [100, 30], 'hrefs per multiget');
            assert(proxy.seen.slice(2).every((s) => s.depth === undefined), 'a multiget must carry no Depth header');
        } finally {
            proxy.server.close();
        }
    });

    await runner.check('directory: a person found by an exact address gets a card MailFlow reads (work e-mail, phones by type, organization); no other person is kept; no search looks like an enumeration', async () => {
        const cards = await until('the directory card', async () => {
            const c = await cardsOf('Directory');
            return c.length === 1 ? c : null;
        }, { timeout: 15000 });
        const p = cards[0].parsed;
        eq(p.displayName, 'Pat Smith');
        eq(p.firstName, 'Pat');
        eq(p.emails.map((e) => [e.value, e.type]), [['pat@corp.example', 'work']]);
        eq(p.phones.map((x) => [x.value, x.type]).sort(), [['+1 555 011 7777', 'work'], ['+1 555 050 7778', 'cell']]);
        eq(p.organization, 'Example Co');
        assert(/^dir-[0-9a-f]{32}$/.test(p.uid), 'uid ' + p.uid);
        const v = cards[0].vcard;
        assert(v.includes('TITLE:Engineer') && v.includes('NOTE:Office: B2'), 'title and office');
        for (const never of ['psmith', '000 0000', 'Patricia', 'Jones']) assert(!v.includes(never), `the card holds ${never}`);
        const g = await bridge.galQueries();
        eq(g.violations || [], [], 'enumeration-like searches');
        assert(g.queries.length >= 1 && g.queries.every((q) => ['pat@corp.example', 'pat', 'Pat Smith'].includes(q)), 'queries ' + JSON.stringify(g.queries));
        eq(g.max_in_flight <= 1, true, 'searches in flight');
    });

    await runner.check('the bridge is read-only: PUT, DELETE, PROPPATCH, MKCOL, COPY and POST to a card and to a book are 405, and nothing changes', async () => {
        const cards = await cardsOf('Exchange contacts');
        const bookPath = new URL(book('Exchange contacts').url).pathname;
        const cardPath = new URL(cards[0].href).pathname;
        const before = await ctagOf(bridge, bookPath);
        for (const method of ['PUT', 'DELETE', 'PROPPATCH', 'MKCOL', 'COPY', 'MOVE', 'POST', 'LOCK', 'PATCH']) {
            for (const p of [cardPath, bookPath, bookPath + 'new.vcf']) {
                const r = await raw(bridge, method, p, { body: 'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:evil\r\nFN:Evil\r\nEND:VCARD\r\n', headers: { 'Content-Type': 'text/vcard', Destination: bookPath + 'x.vcf' } });
                eq(r.status, 405, `${method} ${p}`);
                assert(/PROPFIND/.test(r.headers.allow || ''), 'Allow header');
            }
        }
        eq(await ctagOf(bridge, bookPath), before, 'ctag');
        eq((await cardsOf('Exchange contacts')).length, 2, 'cards');
        const d = await raw(bridge, 'PROPFIND', '/dav/addressbooks/me/', { headers: { Depth: 'infinity' } });
        eq(d.status, 403);
        assert(d.body.includes('propfind-finite-depth'), 'finite depth');
    });

    await runner.check("authentication: a wrong password and a user that is not allowed are 'Authentication failed' to MailFlow's client, with the Basic challenge", async () => {
        let err;
        try {
            await mf.discoverAddressBooks({ serverUrl: root(), username: USER, password: 'definitely-wrong-password', allowPrivate: true });
        } catch (e) {
            err = e;
        }
        assert(err && /Authentication failed/.test(err.message), 'wrong password: ' + (err && err.message));
        err = undefined;
        try {
            await mf.discoverAddressBooks({ serverUrl: root(), username: 'mallory@example.test', password: bridge.password, allowPrivate: true });
        } catch (e) {
            err = e;
        }
        assert(err && /Authentication failed/.test(err.message), 'unknown user: ' + (err && err.message));
        const r = await raw(bridge, 'PROPFIND', '/', { user: 'nobody@example.test', pass: 'x', headers: { Depth: '0' } });
        eq(r.status, 401);
        eq(r.headers['www-authenticate'], 'Basic realm="easbridge", charset="UTF-8"');
        const wk = await raw(bridge, 'GET', '/.well-known/carddav', { user: 'x', pass: 'y' });
        assert(wk.status === 301 && wk.headers.location === '/dav/', 'well-known ' + wk.status);
    });

    await runner.check('/healthz of the contacts bridge shows counts only', async () => {
        const h = await bridge.health();
        eq(h.carddav_listening, true);
        const c = h.accounts[0].contacts;
        assert(c && c.books.length === 3 && c.books.every((b) => b.complete), 'books ' + JSON.stringify(c));
        eq(c.directory_cards, 1);
        const s = JSON.stringify(h);
        for (const bad of ['Pat', 'Lovelace', 'corp.example', ARABIC.first, 'Bulk', 'Clients']) assert(!s.includes(bad), `/healthz shows ${bad}`);
    });

    await runner.check('contacts bridge logs contain no password, no names, no addresses, no numbers, no vCards', async () => {
        const logs = bridge.logs.join('\n');
        assert(logs.length > 500, 'no bridge logs');
        for (const bad of [bridge.password, 'definitely-wrong-password', 'Lovelace', 'Analytical', 'ada@example.test', 'abdulrahman', ARABIC.first, ARABIC.last, ARABIC.company, '+1 555', 'Pat Smith', 'pat@corp.example', 'psmith', 'BEGIN:VCARD', 'Bulk', 'Tower B', 'mallory@example.test', USER]) {
            assert(!logs.includes(bad), `the log contains ${JSON.stringify(bad)}`);
        }
        const b64 = Buffer.from(`${USER}:${bridge.password}`).toString('base64');
        assert(!logs.includes(b64), 'Basic blob in the log');
    });

    await runner.check('the contacts bridge shuts down cleanly on SIGTERM', async () => {
        const res = await bridge.stop();
        eq(res.code, 0, 'exit code');
    });
}

// The automatic certificate: a bridge with no TLS files at all, EASBRIDGE_CA_EXPORT_DIR set, and a
// client process that trusts only the exported CA (NODE_EXTRA_CA_CERTS is read at Node's start, so
// the client is a child process: autotls-client.js). imapflow, nodemailer and MailFlow 3.9.0's real
// CardDAV client connect with certificate verification on.
async function runAutoTlsChecks({ runner, tmp }) {
    const { spawn } = require('node:child_process');
    const bridge = new Bridge();
    const dataDir = path.join(tmp, 'data-autotls');
    const exportDir = path.join(tmp, 'ca-export');
    fs.mkdirSync(dataDir, { recursive: true });
    await bridge.start(null, dataDir, {
        EASBRIDGE_CARDDAV_ADDR: '127.0.0.1:0',
        EASBRIDGE_DIRECTORY_DOMAINS: 'corp.example',
        EASBRIDGE_CA_EXPORT_DIR: exportDir,
    });
    const caFile = path.join(exportDir, 'ca.pem');
    const results = {};
    let childOut = '';
    try {
        await runner.check('automatic certificate: the bridge made its CA and wrote ca.pem to the export directory (0644); the keys stay in the data volume (0600)', async () => {
            assert(fs.existsSync(caFile), 'no ca.pem in the export directory');
            eq((fs.statSync(caFile).mode & 0o777).toString(8), '644', 'export mode');
            eq(fs.readFileSync(caFile, 'utf8'), fs.readFileSync(path.join(dataDir, 'tls', 'ca.pem'), 'utf8'), 'export equals the CA');
            for (const k of ['ca-key.pem', 'server-key.pem']) eq((fs.statSync(path.join(dataDir, 'tls', k)).mode & 0o777).toString(8), '600', k);
            const h = await bridge.health();
            eq(h.tls_mode, 'automatic', 'tls_mode');
            assert(h.tls_server_expires_in_days > 700, 'days left ' + h.tls_server_expires_in_days);
            assert(!bridge.logs.join('\n').includes('PRIVATE KEY'), 'key material in the log');
        });
        // Someone with the CA key mints leaves: one for the name the CA was made for, one for another name.
        const { execFileSync } = require('node:child_process');
        const leafOk = path.join(tmp, 'leaf-ok');
        const leafBad = path.join(tmp, 'leaf-bad');
        execFileSync('go', ['run', './compat/mintleaf', path.join(dataDir, 'tls'), 'easbridge', leafOk], { cwd: path.join(__dirname, '..'), stdio: 'inherit' });
        execFileSync('go', ['run', './compat/mintleaf', path.join(dataDir, 'tls'), 'evil.example.test', leafBad], { cwd: path.join(__dirname, '..'), stdio: 'inherit' });
        const code = await new Promise((resolve, reject) => {
            const child = spawn(process.execPath, [path.join(__dirname, 'autotls-client.js')], {
                env: {
                    ...process.env,
                    NODE_EXTRA_CA_CERTS: caFile, // the only thing this process trusts besides the system roots
                    AUTOTLS_USER: USER, AUTOTLS_PASS: bridge.password, AUTOTLS_LEAF_OK: leafOk, AUTOTLS_LEAF_BAD: leafBad,
                    AUTOTLS_IMAP_HOST: bridge.imap.host, AUTOTLS_IMAP_PORT: String(bridge.imap.port),
                    AUTOTLS_SMTP_HOST: bridge.smtp.host, AUTOTLS_SMTP_PORT: String(bridge.smtp.port),
                    AUTOTLS_CARDDAV_PORT: String(bridge.carddav.port),
                },
                cwd: __dirname,
            });
            child.stdout.on('data', (d) => {
                childOut += d;
            });
            child.stderr.on('data', (d) => {
                childOut += d;
            });
            child.on('error', reject);
            child.on('exit', resolve);
        });
        for (const line of childOut.split('\n')) {
            try {
                const j = JSON.parse(line);
                if (j.name) results[j.name] = j;
            } catch (e) {
                /* not a result line */
            }
        }
        const expect = (name, what) =>
            runner.check(`automatic certificate: ${what}, verification on, trusting only the exported CA`, async () => {
                const r = results[name];
                assert(r && r.ok, `${name}: ${r ? r.detail : 'no result'}\n${childOut}`);
            });
        await expect('imapflow', 'imapflow logs in and lists');
        await expect('nodemailer', 'nodemailer verifies and submits a message');
        await expect('nameconstraints', 'the CA is name-constrained: a leaf it signs for another host name is refused, one for easbridge is accepted');
        await expect('carddav', "MailFlow's real CardDAV client finds the address books");
        await runner.check('automatic certificate: a client that does not trust the exported CA refuses the certificate (the verification is real), and the message was submitted', async () => {
            const tls = require('node:tls');
            const err = await new Promise((resolve) => {
                const s = tls.connect({ host: bridge.imap.host, port: bridge.imap.port, servername: 'easbridge' }, () => {
                    s.destroy();
                    resolve(null);
                });
                s.on('error', resolve);
            });
            assert(err && /certificate|self[- ]signed|unable to verify/i.test(String(err.message)), 'the default trust accepted the certificate: ' + err);
            eq(code, 0, 'client exit code');
            const sent = await bridge.sent();
            assert(sent.length >= 1, 'the message submitted over the verified connection did not arrive');
        });
    } finally {
        await bridge.stop().catch(() => {});
    }
}

// The opt-in full sweep: a bridge with EASBRIDGE_DIRECTORY_FULL=on and a generated directory of a
// few thousand entries (strict ANR matching, a server that answers from one character); MailFlow's real client lists the whole Directory book.
async function runFullDirectoryChecks({ runner, tmp }) {
    mapEasbridgeHost();
    const mf = await loadMailflow();
    const bridge = new Bridge();
    const dataDir = path.join(tmp, 'data-fulldir');
    fs.mkdirSync(dataDir, { recursive: true });
    await bridge.start(path.join(tmp, 'tls'), dataDir, {
        EASBRIDGE_CARDDAV_ADDR: '127.0.0.1:0',
        EASBRIDGE_DIRECTORY_DOMAINS: 'corp.example',
        EASBRIDGE_DIRECTORY_FULL: 'on',
        EASBRIDGE_DIRECTORY_LOOKUPS_PER_HOUR: '180',
        EASBRIDGE_FAKE_SEARCH_SPACING: '1ms',
    });
    console.log(`full directory bridge: carddav ${bridge.addr.carddav}`);
    const creds = () => ({ username: USER, password: bridge.password, allowPrivate: true });
    await bridge.control('POST', '/gal-anr', { query: { on: '1', min: '1' } });
    const gen = await bridge.control('POST', '/gal-generate', { query: { n: '3000', seed: '3', domains: 'corp.example', external: 'outside.example' } });
    const wanted = gen.in_domain + 1; // and the account's own entry
    await bridge.galAdd({ DisplayName: 'Compat Own', FirstName: 'Compat', LastName: 'Own', EmailAddress: 'compat@corp.example' });
    let dirUrl;

    await runner.check(`full sweep: MailFlow's real client lists a Directory book of ${wanted} cards, every one of the directory domain, none of the external contacts`, async () => {
        const books = await until('the directory book', async () => {
            try {
                const b = await mf.discoverAddressBooks({ serverUrl: `https://easbridge:${bridge.carddav.port}/`, ...creds() });
                return b.find((x) => x.displayName === 'Directory') ? b : null;
            } catch (e) {
                return null;
            }
        }, { timeout: 15000 });
        dirUrl = books.find((b) => b.displayName === 'Directory').url;
        const cards = await until('the sweep to fill the book', async () => {
            const c = await mf.fetchAddressBookCards({ url: dirUrl, ...creds() });
            return c.length === wanted ? c : null;
        }, { timeout: 90000, step: 500 });
        const parsed = cards.map((c) => mf.parseVCard(c.vcard));
        eq(new Set(parsed.map((p) => p.uid)).size, wanted, 'distinct uids');
        assert(parsed.every((p) => /^dir-[0-9a-f]{32}$/.test(p.uid)), 'uids');
        assert(parsed.every((p) => p.emails.length === 1 && p.emails[0].value.endsWith('@corp.example') && p.emails[0].type === 'work'), 'e-mail addresses and types');
        assert(!cards.some((c) => c.vcard.includes('outside.example')), 'an external contact is in the book');
        const arabic = parsed.filter((p) => /[\u0600-\u06FF]/.test(p.displayName || ''));
        assert(arabic.length > 100, `${arabic.length} Arabic display names`);
        assert(arabic.every((p) => /^[a-z0-9._-]+@corp\.example$/.test(p.emails[0].value)), 'Arabic names carry Latin addresses');
        assert(parsed.filter((p) => p.phones.length === 2 && p.phones.some((x) => x.type === 'cell')).length >= wanted - 1, 'phones (the account\'s own entry has none)');
        assert(!cards.some((c) => c.vcard.includes('+1 555 055 0000')), 'the home phone is not kept');
    });

    await runner.check('full sweep: the same book through the PROPFIND and addressbook-multiget path (batches of 100)', async () => {
        const proxy = await startRefusingProxy(bridge, path.join(tmp, 'tls'));
        try {
            const proxied = dirUrl.replace(`:${bridge.carddav.port}/`, `:${proxy.port}/`);
            const cards = await mf.fetchAddressBookCards({ url: proxied, ...creds() });
            eq(cards.length, wanted, 'cards');
            const multigets = proxy.seen.filter((s) => s.method === 'REPORT' && s.body.includes('addressbook-multiget'));
            eq(multigets.length, Math.ceil(wanted / 100), 'multiget batches');
            assert(multigets.every((m) => m.depth === undefined), 'no Depth on a multiget');
        } finally {
            proxy.server.close();
        }
    });

    await runner.check('full sweep: /healthz shows the sweep as counts, searches stayed within the cap, no query looked like a wildcard, the logs hold no people', async () => {
        const h = await bridge.health();
        const sw = h.accounts[0].contacts.directory_sweep;
        assert(sw && sw.full === true && sw.state === 'waiting' && sw.calibrated_length === 1 && sw.last_sweep_capped === false && sw.prefixes_queued === 0, JSON.stringify(sw));
        assert(sw.searches_this_sweep > 100 && sw.searches_this_sweep <= 5000, 'searches ' + sw.searches_this_sweep);
        eq(sw.entries_found_this_sweep, wanted);
        const g = await bridge.galQueries();
        eq(g.queries[0], 'c', 'the calibration starts with the first character of the own local part');
        assert(g.queries.every((q) => q.length >= 1 && q.length <= 20 && /^[a-z0-9._-]+$/.test(q)), 'a query outside the alphabet of aliases');
        eq(g.max_in_flight <= 1, true, 'searches in flight');
        const logs = bridge.logs.join('\n');
        for (const bad of [bridge.password, 'Compat Own', 'compat@corp.example', 'outside.example', 'alsulami', 'BEGIN:VCARD']) assert(!logs.includes(bad), `the log contains ${JSON.stringify(bad)}`);
        for (const want of ['directory sweep started', 'directory sweep calibrated', 'directory sweep finished']) assert(logs.includes(want), 'no log line ' + want);
    });

    await runner.check('the full-directory bridge shuts down cleanly on SIGTERM', async () => {
        const res = await bridge.stop();
        eq(res.code, 0, 'exit code');
    });
}

module.exports = { runContactsChecks, runFullDirectoryChecks, runAutoTlsChecks, mapEasbridgeHost };
