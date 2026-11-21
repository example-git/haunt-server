"""Serve only the bundled search API on the private socket inherited from Haunt."""
import socket
from socketserver import ThreadingMixIn
from wsgiref.simple_server import WSGIRequestHandler, WSGIServer
from searx.webapp import app


class LocalServer(ThreadingMixIn, WSGIServer):
    daemon_threads = True

    def get_request(self):
        connection, _ = self.socket.accept()
        return connection, ("127.0.0.1", 0)


class QuietHandler(WSGIRequestHandler):
    def log_message(self, format, *args):
        pass


def search(environ, start_response):
    if environ["PATH_INFO"] == "/search" and environ["REQUEST_METHOD"] == "GET":
        return app(environ, start_response)
    start_response("404 Not Found", [("Content-Length", "0")])
    return []


with LocalServer(("127.0.0.1", 0), QuietHandler, bind_and_activate=False) as server:
    server.socket.close()
    server.socket = socket.socket(fileno=3)
    server.server_name = "localhost"
    server.server_port = 80
    server.setup_environ()
    server.set_app(search)
    server.serve_forever()
