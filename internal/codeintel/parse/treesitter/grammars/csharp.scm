; C# symbol + call + import captures. See rust.scm for the capture
; vocabulary. The backend dedups by span keeping the more specific kind.

(class_declaration name: (identifier) @name) @definition.class
(interface_declaration name: (identifier) @name) @definition.interface
(struct_declaration name: (identifier) @name) @definition.class
(enum_declaration name: (identifier) @name) @definition.class
(record_declaration name: (identifier) @name) @definition.class

(method_declaration name: (identifier) @name) @definition.method
(constructor_declaration name: (identifier) @name) @definition.method

(using_directive (qualified_name) @_import.path)
(using_directive (identifier) @_import.path)

(invocation_expression function: (identifier) @name) @reference.call
(invocation_expression
  function: (member_access_expression name: (identifier) @name)) @reference.call
