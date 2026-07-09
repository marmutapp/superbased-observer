; Lua symbol + call + import captures (Extended tier — S+I, best-effort C).
; See rust.scm for the capture vocabulary. Table/method functions
; (M.foo / M:foo) are captured via their field/method identifier. require()
; uses the #eq? predicate so only require args become imports.

(function_declaration name: (identifier) @name) @definition.function
(function_declaration name: (dot_index_expression field: (identifier) @name)) @definition.function
(function_declaration name: (method_index_expression method: (identifier) @name)) @definition.method

(function_call name: (identifier) @name) @reference.call
(function_call name: (dot_index_expression field: (identifier) @name)) @reference.call
(function_call name: (method_index_expression method: (identifier) @name)) @reference.call

(function_call
  name: (identifier) @_req
  arguments: (arguments (string) @_import.path)
  (#eq? @_req "require"))
