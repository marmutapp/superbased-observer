export type NavIcon =
  | "overview"
  | "live"
  | "search"
  | "sessions"
  | "actions"
  | "security"
  | "cost"
  | "analysis"
  | "tools"
  | "compression"
  | "cache"
  | "suggestions"
  | "discovery"
  | "patterns"
  | "privacy"
  | "trajectories"
  | "evals"
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
      { id: "live", label: "Live", path: "/live", icon: "live" },
      { id: "sessions", label: "Sessions", path: "/sessions", icon: "sessions" },
      { id: "actions", label: "Actions", path: "/actions", icon: "actions" },
      { id: "trajectories", label: "Trajectories", path: "/trajectories", icon: "trajectories" },
      { id: "evals", label: "Evals", path: "/evals", icon: "evals" },
      { id: "security", label: "Security", path: "/security", icon: "security" },
      { id: "search", label: "Search", path: "/search", icon: "search" },
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
      { id: "cache", label: "Cache", path: "/cache", icon: "cache" },
      { id: "suggestions", label: "Suggestions", path: "/suggestions", icon: "suggestions" },
      { id: "routing", label: "Routing", path: "/routing", icon: "analysis" },
      { id: "discovery", label: "Discovery", path: "/discovery", icon: "discovery" },
      { id: "patterns", label: "Patterns", path: "/patterns", icon: "patterns" },
    ],
  },
  {
    id: "configure",
    label: "Configure",
    items: [
      { id: "privacy", label: "Privacy", path: "/privacy", icon: "privacy" },
      { id: "settings", label: "Settings", path: "/settings", icon: "settings" },
    ],
  },
];

export const NAV_ITEMS: NavItem[] = NAV_GROUPS.flatMap((g) => g.items);
