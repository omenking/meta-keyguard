"""Chrome native messaging framing: a 32-bit length in native byte order, then UTF-8 JSON."""
import json
import struct

HEADER = struct.Struct('=I')
# Chrome refuses host-to-extension messages larger than 1 MB.
MAX_OUTGOING = 1024 * 1024


def read_message(stream):
    """Read one message, or return None when Chrome has closed the pipe."""
    header = stream.read(HEADER.size)
    if len(header) < HEADER.size:
        return None
    (length,) = HEADER.unpack(header)
    body = stream.read(length)
    if len(body) < length:
        return None
    return json.loads(body.decode('utf-8'))


def write_message(stream, message):
    body = json.dumps(message, separators=(',', ':')).encode('utf-8')
    if len(body) > MAX_OUTGOING:
        raise ValueError('Native message exceeds the 1 MB limit')
    stream.write(HEADER.pack(len(body)) + body)
    stream.flush()
