# pic-sure doctor

Check the host, Docker, the stack and the network

Check the host, Docker, the stack and the network.

Without a stack (no --stack, and none at or above the current directory)
only the host and Docker are checked. Exits 1 if any check fails.

```
pic-sure doctor [flags]
```

## Flags

```
      --network   also check reachability through the proxy
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure`](pic-sure.md)
