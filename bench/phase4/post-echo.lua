-- POST /echo with a 4 KiB body, for wrk -s. The body streams through the
-- proxy's request-writer goroutine and back, on reused connections.
wrk.method = "POST"
wrk.path = "/echo"
wrk.body = string.rep("0123456789abcdef", 256)
wrk.headers["Content-Type"] = "application/octet-stream"
