export type NavIcon =
  | "overview"
  | "sessions"
  | "actions"
  | "cost"
  | "analysis"
  | "tools"
  | "compression"
  | "discovery"
  | "patterns"
  | "settings";

export type NavItem = {
  id: string;
  label: string;
  path: string;
  icon: NavIcon;
};

export type NavGroup = {
  id: string;
  label: string;
  items: NavItem[];
};

export const NAV_GROUPS: NavGroup[] = [
  {
    id: "monitor",
    label: "Monitor",
    items: [
      { id: "overview", label: "Overview", path: "/", icon: "overview" },
      { id: "sessions", label: "Sessions", path: "/sessions", icon: "sessions" },
      { id: "actions", label: "Actions", path: "/actions", icon: "actions" },
    ],
  },
  {
    id: "analyze",
    label: "Analyze",
    items: [
      { id: "cost", label: "Cost", path: "/cost", icon: "cost" },
      { id: "analysis", label: "Analysis", path: "/analysis", icon: "analysis" },
      { id: "tools", label: "Tools", path: "/tools", icon: "tools" },
    ],
  },
  {
    id: "optimize",
    label: "Optimize",
    items: [
      { id: "compression", label: "Compression", path: "/compression", icon: "compression" },
      { id: "discovery", label: "Discovery", path: "/discovery", icon: "discovery" },
      { id: "patterns", label: "Patterns", path: "/patterns", icon: "patterns" },
    ],
  },
  {
    id: "configure",
    label: "Configure",
    items: [{ id: "settings", label: "Settings", path: "/settings", icon: "settings" }],
  },
];

export const NAV_ITEMS: NavItem[] = NAV_GROUPS.flatMap((g) => g.items);
