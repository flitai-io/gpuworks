import { Card, CardFooter, CardHeader } from "../../../components/ui/card";
import { Skeleton } from "../../../components/ui/skeleton";

const titleWidths = ["w-3/5", "w-2/5", "w-3/4", "w-1/2", "w-2/3", "w-3/5"];

export function CatalogResultsSkeleton({ cards }: { cards: boolean }) {
  return (
    <div aria-hidden="true" className={cards ? "model-tile-grid discovery-tiles max-h-full overflow-hidden" : "max-h-full overflow-hidden"}>
      {titleWidths.map((width, i) => cards ? (
        <Card key={i} className="min-w-0 border border-border-subtle">
          <CardHeader>
            <div className="flex items-center gap-3">
              <Skeleton className="size-10 shrink-0" />
              <div className="grid flex-1 gap-2">
                <Skeleton className={`h-3 ${width}`} />
                <Skeleton className="h-2 w-1/4" />
              </div>
            </div>
          </CardHeader>
          <CardFooter><Skeleton className="h-2 w-1/3" /></CardFooter>
        </Card>
      ) : (
        <div key={i} className="live-card">
          <div className="live-card-title">
            <Skeleton className="size-6 shrink-0" />
            <Skeleton className={`h-3 ${width}`} />
          </div>
          <div className="mt-1 flex h-5 items-center justify-between">
            <Skeleton className="h-2 w-1/4" />
            <Skeleton className="h-2 w-1/5" />
          </div>
        </div>
      ))}
    </div>
  );
}

export function CatalogDetailSkeleton() {
  return (
    <div className="live-readme" aria-hidden="true">
      <div className="model-profile">
        <div className="flex items-center gap-2">
          <Skeleton className="size-control-lg shrink-0" />
          <div className="grid flex-1 gap-2">
            <Skeleton className="h-4 w-2/5" />
            <Skeleton className="h-2 w-1/4" />
          </div>
        </div>
        <div className="mt-2 flex gap-2">
          <Skeleton className="h-4 w-16" />
          <Skeleton className="h-4 w-20" />
          <Skeleton className="h-4 w-12" />
        </div>
      </div>
      <div className="mt-5 grid gap-3">
        <Skeleton className="mb-1 h-3 w-1/4" />
        <Skeleton className="h-2 w-11/12" />
        <Skeleton className="h-2 w-4/5" />
      </div>
    </div>
  );
}
