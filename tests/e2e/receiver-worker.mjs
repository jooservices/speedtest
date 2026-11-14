// A webhook receiver that runs in its own thread: the scenarios block their event loop with synchronous psql calls, and a receiver on
// that loop could not answer the center in time, so a delivery attempt failed and its retry came a minute later than the scenario waits.
import http from 'node:http';
import { parentPort } from 'node:worker_threads';

const server = http.createServer((req, res) => {
  const chunks = [];
  req.on('data', chunk => chunks.push(chunk));
  req.on('end', () => {
    let body;
    try { body = JSON.parse(Buffer.concat(chunks).toString()); } catch { body = { raw: true }; }
    res.writeHead(200);
    res.end('ok');
    parentPort.postMessage({ body });
  });
});
server.listen(0, '127.0.0.1', () => parentPort.postMessage({ port: server.address().port }));
parentPort.on('message', message => { if (message === 'close') { server.close(); parentPort.close(); } });
