# pic-sure dev on

Run SERVICE from local source

Build SERVICE's component from its local checkout
(components.<component>.source), add SERVICE to dev.services, re-render and
recreate SERVICE. psama, hpds, gateway, operations and query also get a
JDWP debug port on 127.0.0.1, from the stack's network.dev_ports block;
dev list shows it. On a stopped stack, pic-sure up starts it.

Run it again after changing the source to rebuild and recreate.

```
pic-sure dev on SERVICE
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure dev`](pic-sure_dev.md)
