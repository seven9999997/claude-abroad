#!/usr/bin/env python3
# M2 真机验证第 8 步的长连接测试程序（scripts/m2/run.sh 复制到状态目录后，以用户身份运行）。
import socket, sys, time, os
# 用法：longconn.py ext <host:port> <状态文件> <PID 文件> | lo <端口> <状态文件> <PID 文件>
# 自己把 PID 写进 PID 文件（restore.sh 按它结束本进程）；收到 SIGUSR1 时再发一次请求，把“ok”或“broken”写进状态文件。
import signal
mode, target, st, pidf = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
open(pidf, "w").write(str(os.getpid()))
if mode == "lo":
    srv = socket.socket(); srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("127.0.0.1", int(target))); srv.listen(1)
    if os.fork() == 0:
        c, _ = srv.accept()
        while True:
            d = c.recv(64)
            if not d: break
            c.sendall(d)
        os._exit(0)
    c = socket.create_connection(("127.0.0.1", int(target)), timeout=5)
    def ping():
        c.sendall(b"x"); return c.recv(1) == b"x"
else:
    h, p = target.rsplit(":", 1)
    c = socket.create_connection((h, int(p)), timeout=8)
    req = ("GET / HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive\r\n\r\n" % h).encode()
    def ping():
        c.sendall(req); return len(c.recv(256)) > 0
def check(*_):
    try: r = "ok" if ping() else "broken"
    except Exception: r = "broken"
    open(st, "a").write(r + "\n")
signal.signal(signal.SIGUSR1, check)
check()
while True: time.sleep(1)
