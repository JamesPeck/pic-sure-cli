# pic-sure config edit

Edit the config in $EDITOR, validating on save

```text
Open a copy of pic-sure.yaml in $VISUAL or $EDITOR (vi if neither is set).
When the editor exits, the copy is validated and saved over pic-sure.yaml.
If it's invalid, the editor reopens with the problems listed at the top;
exit without saving to give up. Needs a terminal.
```

```
pic-sure config edit
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure config`](pic-sure_config.md)
