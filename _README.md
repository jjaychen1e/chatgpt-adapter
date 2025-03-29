# How to debug

```bash
go build -gcflags=all="-N -l" -toolexec iocgo ./main.go
```

```bash
dlv --listen=:2345 --headless --api-version=2 --accept-multiclient 
exec ./main
```

Then use VS Code `launch.json` to attach to the process.