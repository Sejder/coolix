#!/usr/bin/env node
const http = require("node:http");

const port = process.env.PORT || 8080;

http
  .createServer((req, res) => {
    if (req.url === "/healthz") return res.end("ok\n");
    if (req.url === "/") return res.end(`Hello from coolix! (slot ${process.env.COOLIX_SLOT})\n`);
    res.statusCode = 404;
    res.end("not found\n");
  })
  .listen(port, "127.0.0.1", () => console.log(`listening on 127.0.0.1:${port}`));
