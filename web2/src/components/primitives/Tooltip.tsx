import {
  arrow,
  autoUpdate,
  flip,
  FloatingArrow,
  FloatingPortal,
  offset,
  shift,
  useDismiss,
  useFloating,
  useFocus,
  useHover,
  useInteractions,
  useMergeRefs,
  useRole,
  useTransitionStyles,
} from "@floating-ui/react";
import clsx from "clsx";
import {
  cloneElement,
  forwardRef,
  isValidElement,
  useMemo,
  useRef,
  useState,
  type HTMLAttributes,
  type ReactElement,
  type ReactNode,
} from "react";

// MIRROR of web/src/components/primitives/Tooltip.tsx — the dashboard's single
// explanatory-popover primitive. Themed surface (--bg-3 / --line-3 / --shadow-2
// / --accent-ring focus); hover OR focus opens, leave / blur / escape closes.
//
// The child is cloned with the floating refs + interaction listeners, so it
// must be a single React element that accepts ref + spread props. String /
// fragment children are not supported — wrap them in a span.

type TooltipSide = "top" | "bottom" | "left" | "right";

export interface TooltipProps {
  content: ReactNode;
  children: ReactElement;
  side?: TooltipSide;
  offset?: number;
  maxWidth?: number;
  arrow?: boolean;
  open?: boolean;
  delay?: number;
  tone?: "default" | "accent" | "danger" | "success";
  disabled?: boolean;
}

const toneSurface: Record<NonNullable<TooltipProps["tone"]>, string> = {
  default: "bg-bg-3 text-fg-1 border-line-3",
  accent: "bg-[var(--accent-soft)] text-fg-0 border-[var(--accent-ring)]",
  danger: "bg-[rgba(239,68,68,0.10)] text-fg-0 border-[rgba(239,68,68,0.40)]",
  success: "bg-[rgba(34,197,94,0.10)] text-fg-0 border-[rgba(34,197,94,0.40)]",
};

const toneFill: Record<NonNullable<TooltipProps["tone"]>, string> = {
  default: "var(--bg-3)",
  accent: "var(--accent-soft)",
  danger: "rgba(239,68,68,0.10)",
  success: "rgba(34,197,94,0.10)",
};

const toneStroke: Record<NonNullable<TooltipProps["tone"]>, string> = {
  default: "var(--line-3)",
  accent: "var(--accent-ring)",
  danger: "rgba(239,68,68,0.40)",
  success: "rgba(34,197,94,0.40)",
};

export function Tooltip({
  content,
  children,
  side = "top",
  offset: gap = 6,
  maxWidth = 280,
  arrow: showArrow = true,
  open: controlledOpen,
  delay = 200,
  tone = "default",
  disabled = false,
}: TooltipProps) {
  const [uncontrolledOpen, setUncontrolledOpen] = useState(false);
  const arrowRef = useRef<SVGSVGElement | null>(null);

  const open = controlledOpen !== undefined ? controlledOpen : uncontrolledOpen;
  const setOpen = (v: boolean) => {
    if (controlledOpen === undefined) setUncontrolledOpen(v);
  };

  const { refs, floatingStyles, context } = useFloating({
    open,
    onOpenChange: setOpen,
    placement: side,
    middleware: [
      offset(gap),
      flip({ fallbackAxisSideDirection: "start" }),
      shift({ padding: 8 }),
      ...(showArrow ? [arrow({ element: arrowRef })] : []),
    ],
    whileElementsMounted: autoUpdate,
  });

  const hover = useHover(context, {
    move: false,
    enabled: !disabled && controlledOpen === undefined,
    restMs: delay,
    delay: { close: 80 },
  });
  const focus = useFocus(context, {
    enabled: !disabled && controlledOpen === undefined,
  });
  const dismiss = useDismiss(context);
  const role = useRole(context, { role: "tooltip" });

  const { getReferenceProps, getFloatingProps } = useInteractions([
    hover,
    focus,
    dismiss,
    role,
  ]);

  const { isMounted, styles: transitionStyles } = useTransitionStyles(context, {
    duration: { open: 120, close: 80 },
    initial: { opacity: 0, transform: "scale(0.96)" },
    open: { opacity: 1, transform: "scale(1)" },
    close: { opacity: 0, transform: "scale(0.96)" },
  });

  const childRef = (children as { ref?: React.Ref<unknown> }).ref;
  const mergedRef = useMergeRefs([refs.setReference, childRef ?? null]);

  const trigger = useMemo(() => {
    if (!isValidElement(children)) return children;
    const childProps = children.props as HTMLAttributes<HTMLElement>;
    return cloneElement(children, {
      ref: mergedRef,
      ...getReferenceProps({
        ...childProps,
      }),
    } as Partial<typeof childProps> & { ref: typeof mergedRef });
  }, [children, getReferenceProps, mergedRef]);

  if (disabled || content == null || content === false) {
    return children;
  }

  return (
    <>
      {trigger}
      {isMounted && (
        <FloatingPortal>
          <div
            ref={refs.setFloating}
            style={floatingStyles}
            {...getFloatingProps()}
            className="pointer-events-none z-[80]"
          >
            <div
              style={{ ...transitionStyles, maxWidth }}
              role="tooltip"
              className={clsx(
                "rounded-md border px-2.5 py-1.5 text-xs leading-relaxed shadow-[var(--shadow-2)]",
                "[&_kbd]:rounded [&_kbd]:bg-bg-4 [&_kbd]:px-1.5 [&_kbd]:py-0.5 [&_kbd]:font-mono [&_kbd]:text-[10px]",
                "[&_code]:rounded [&_code]:bg-bg-4 [&_code]:px-1 [&_code]:font-mono [&_code]:text-[11px]",
                "[&_strong]:font-semibold [&_strong]:text-fg-0",
                toneSurface[tone],
              )}
            >
              {content}
              {showArrow && (
                <FloatingArrow
                  ref={arrowRef}
                  context={context}
                  width={10}
                  height={5}
                  fill={toneFill[tone]}
                  stroke={toneStroke[tone]}
                  strokeWidth={1}
                />
              )}
            </div>
          </div>
        </FloatingPortal>
      )}
    </>
  );
}

// TooltipSpan — convenience wrapper that adds a tooltip to bare text.
export const TooltipSpan = forwardRef<
  HTMLSpanElement,
  TooltipProps & { className?: string }
>(function TooltipSpan({ content, children, className, ...rest }, ref) {
  return (
    <Tooltip content={content} {...rest}>
      <span ref={ref} className={className} tabIndex={0}>
        {children}
      </span>
    </Tooltip>
  );
});
