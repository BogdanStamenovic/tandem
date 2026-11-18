import asyncio
async def pipe(r,w):
    try:
        while (b:=await r.read(262144)): w.write(b); await w.drain()
    finally: w.close()
async def handle(cr,cw):
    line=await cr.readline(); host,port=line.split()[1].decode().rsplit(':',1)
    while (await cr.readline()) not in (b'\r\n',b''): pass
    ur,uw=await asyncio.open_connection(host,int(port))
    cw.write(b'HTTP/1.1 200 OK\r\n\r\n'); await cw.drain()
    await asyncio.gather(pipe(cr,uw),pipe(ur,cw),return_exceptions=True)
async def main():
    s=await asyncio.start_server(handle,'192.168.1.139',18778); await s.serve_forever()
asyncio.run(main())
