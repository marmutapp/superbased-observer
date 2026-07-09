; Ruby symbol + call + import captures. See rust.scm for the capture
; vocabulary. require/require_relative are method calls gated by #any-of? so
; only those string args are captured as imports (not every call arg).

(method name: (identifier) @name) @definition.method
(singleton_method name: (identifier) @name) @definition.method
(class name: (constant) @name) @definition.class
(module name: (constant) @name) @definition.module

(call method: (identifier) @name) @reference.call

(call
  method: (identifier) @_req
  arguments: (argument_list (string (string_content) @_import.path))
  (#any-of? @_req "require" "require_relative"))
