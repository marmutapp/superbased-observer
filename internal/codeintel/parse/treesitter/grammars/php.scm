; PHP symbol + call + import captures. See rust.scm for the capture
; vocabulary. Traits map to the interface kind. `use` clauses yield imports;
; the backend dedups definitions by span keeping the more specific kind.

(function_definition name: (name) @name) @definition.function
(method_declaration name: (name) @name) @definition.method
(class_declaration name: (name) @name) @definition.class
(interface_declaration name: (name) @name) @definition.interface
(trait_declaration name: (name) @name) @definition.interface
(enum_declaration name: (name) @name) @definition.class

(namespace_use_clause (qualified_name) @_import.path)
(namespace_use_clause (name) @_import.path)

(function_call_expression function: (name) @name) @reference.call
(member_call_expression name: (name) @name) @reference.call
(scoped_call_expression name: (name) @name) @reference.call
