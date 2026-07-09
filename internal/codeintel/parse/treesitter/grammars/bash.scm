; Bash symbol + call captures (Extended tier — S + best-effort C; no imports).
; See rust.scm for the capture vocabulary. Every invoked command is a
; best-effort call site (the name node is the command word).

(function_definition name: (word) @name) @definition.function

(command name: (command_name) @name) @reference.call
