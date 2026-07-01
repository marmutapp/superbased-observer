import { AlertTriangle, Info, Lightbulb } from "lucide-react";
import { api, type Suggestion } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { Card, ErrorState, PageHeader, Spinner } from "@/components/ui";
import { Pill } from "@/components/primitives";

export function SuggestionsPage() {
  const { days } = useFilters();
  const { data, error, loading, reload } = useApi(() => api.suggestions(days), [days]);

  return (
    <>
      <PageHeader
        title="Suggestions"
        subtitle="Org-wide cost and hygiene advisories, computed read-side from the same content-free metrics the dashboard shows. Nothing is stored or sent anywhere."
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <Card className="p-4">
          <Spinner label="Analyzing…" />
        </Card>
      ) : data.suggestions.length === 0 ? (
        <Card className="p-6">
          <div className="flex items-center gap-2 text-[15px] font-semibold text-fg-0">
            <Lightbulb className="h-4 w-4 text-accent" />
            Nothing to flag
          </div>
          <p className="mt-2 max-w-2xl text-sm text-fg-2">
            No cost-leak or hygiene signal crossed a threshold for this window —
            spend is reasonably distributed, capture is healthy, and cache reuse
            looks fine. Advisories appear here as the org grows.
          </p>
        </Card>
      ) : (
        <div className="space-y-3">
          {data.suggestions.map((s) => (
            <SuggestionCard key={s.id} s={s} />
          ))}
        </div>
      )}
    </>
  );
}

function SuggestionCard({ s }: { s: Suggestion }) {
  const warn = s.severity === "warn";
  return (
    <Card className={warn ? "border-warn/40 p-4" : "p-4"}>
      <div className="flex items-start gap-3">
        <div className={`mt-0.5 shrink-0 ${warn ? "text-warn" : "text-accent"}`}>
          {warn ? <AlertTriangle size={16} /> : <Info size={16} />}
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-[13.5px] font-semibold text-fg-0">{s.title}</span>
            {s.metric && <Pill variant={warn ? "warn" : "neutral"}>{s.metric}</Pill>}
          </div>
          <p className="mt-1 text-[12.5px] leading-relaxed text-fg-2">{s.detail}</p>
        </div>
      </div>
    </Card>
  );
}
