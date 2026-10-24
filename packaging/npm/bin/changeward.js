#!/usr/bin/env node
// Thin launcher: downloads the Changeward binary for this platform from the
// GitHub release matching this package's version, verifies its SHA-256
// against the release's checksums.txt, caches it, and runs it with stdio
// inherited (so `npx @changeward/cli mcp postgres` works as an MCP server).
'use strict';
const { spawnSync } = require('child_process');
const crypto = require('crypto');
const fs = require('fs');
const https = require('https');
const os = require('os');
const path = require('path');
const zlib = require('zlib');

const version = require('../package.json').version;
const base = process.env.CHANGEWARD_DOWNLOAD_BASE || `https://github.com/srawlin/changeward/releases/download/v${version}`;
const platform = { darwin: 'darwin', linux: 'linux' }[process.platform];
const arch = { x64: 'amd64', arm64: 'arm64' }[process.arch];

function fail(msg) {
  process.stderr.write(`changeward launcher: ${msg}\n`);
  process.exit(1);
}

function get(url, redirects = 5) {
  if (url.startsWith('file://')) return Promise.resolve(fs.readFileSync(url.slice(7)));
  return new Promise((resolve, reject) => {
    const mod = url.startsWith('http://') ? require('http') : https;
    mod.get(url, { headers: { 'User-Agent': 'changeward-npm' } }, (res) => {
      if ([301, 302, 303, 307, 308].includes(res.statusCode) && res.headers.location && redirects > 0) {
        res.resume();
        return resolve(get(new URL(res.headers.location, url).toString(), redirects - 1));
      }
      if (res.statusCode !== 200) { res.resume(); return reject(new Error(`GET ${url}: HTTP ${res.statusCode}`)); }
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => resolve(Buffer.concat(chunks)));
    }).on('error', reject);
  });
}

// Minimal ustar reader: returns the contents of the named entry.
function untar(buf, name) {
  for (let off = 0; off + 512 <= buf.length;) {
    const header = buf.subarray(off, off + 512);
    if (header.every((b) => b === 0)) break;
    const entry = header.subarray(0, 100).toString('utf8').replace(/\0.*$/s, '');
    const prefix = header.subarray(345, 500).toString('utf8').replace(/\0.*$/s, '');
    const size = parseInt(header.subarray(124, 136).toString('utf8').replace(/\0.*$/s, '').trim() || '0', 8);
    const full = prefix ? `${prefix}/${entry}` : entry;
    off += 512;
    if (full === name || full === `./${name}`) return buf.subarray(off, off + size);
    off += Math.ceil(size / 512) * 512;
  }
  return null;
}

async function ensureBinary() {
  if (process.env.CHANGEWARD_BINARY) return process.env.CHANGEWARD_BINARY;
  if (!platform || !arch) fail(`unsupported platform ${process.platform}/${process.arch}`);
  const cacheRoot = process.env.CHANGEWARD_CACHE || path.join(process.env.XDG_CACHE_HOME || path.join(os.homedir(), '.cache'), 'changeward');
  const dir = path.join(cacheRoot, version);
  const bin = path.join(dir, 'changeward');
  if (fs.existsSync(bin)) return bin;
  const asset = `changeward_${platform}_${arch}.tar.gz`;
  const [tgz, sums] = await Promise.all([get(`${base}/${asset}`), get(`${base}/checksums.txt`)]);
  const want = sums.toString('utf8').split('\n').map((l) => l.trim().split(/\s+/)).find((p) => p[1] === asset);
  if (!want) fail(`no checksum for ${asset}`);
  const got = crypto.createHash('sha256').update(tgz).digest('hex');
  if (got !== want[0]) fail(`checksum mismatch for ${asset}: got ${got}, want ${want[0]}`);
  const data = untar(zlib.gunzipSync(tgz), 'changeward');
  if (!data) fail(`changeward not found in ${asset}`);
  fs.mkdirSync(dir, { recursive: true });
  const tmp = `${bin}.${process.pid}.tmp`;
  fs.writeFileSync(tmp, data, { mode: 0o755 });
  fs.renameSync(tmp, bin);
  return bin;
}

ensureBinary().then((bin) => {
  const r = spawnSync(bin, process.argv.slice(2), { stdio: 'inherit' });
  if (r.error) fail(r.error.message);
  process.exit(r.status === null ? 1 : r.status);
}).catch((e) => fail(e.message));
