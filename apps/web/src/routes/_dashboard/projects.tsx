import { createFileRoute, Link } from "@tanstack/react-router";
import { ChevronRight, FolderKanban, Plus } from "lucide-react";
import { useState } from "react";
import { EmptyState } from "@/components/empty-state";
import { LoadingScreen } from "@/components/loading-screen";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useCreateProject, useProjects } from "@/hooks/use-projects";

export const Route = createFileRoute("/_dashboard/projects")({
  component: ProjectsPage,
});

function ProjectsPage() {
  const { data: projects, isLoading } = useProjects();
  const createProject = useCreateProject();
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    await createProject.mutateAsync({ name, description });
    setName("");
    setDescription("");
    setOpen(false);
  }

  if (isLoading) return <LoadingScreen />;

  return (
    <div>
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">Projects</h1>
          <p className="text-sm text-muted-foreground">Manage your projects</p>
        </div>
        <Button onClick={() => setOpen(true)}>
          <Plus className="h-4 w-4" /> New Project
        </Button>
      </div>

      <Dialog
        open={open}
        onOpenChange={(v) => {
          setOpen(v);
          if (!v) {
            setName("");
            setDescription("");
          }
        }}
      >
        <DialogContent>
          <form onSubmit={handleCreate}>
            <DialogHeader>
              <DialogTitle>New Project</DialogTitle>
              <DialogDescription>
                Projects group your applications, databases, and cron jobs.
              </DialogDescription>
            </DialogHeader>
            <div className="mt-4 space-y-4">
              <div className="space-y-2">
                <Label>Name</Label>
                <Input
                  placeholder="my-project"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  required
                  autoFocus
                />
              </div>
              <div className="space-y-2">
                <Label>Description</Label>
                <Input
                  placeholder="Optional description"
                  value={description}
                  onChange={(e) => setDescription(e.target.value)}
                />
              </div>
            </div>
            <DialogFooter className="mt-6">
              <Button type="button" variant="outline" onClick={() => setOpen(false)}>
                Cancel
              </Button>
              <Button type="submit" disabled={createProject.isPending || !name.trim()}>
                {createProject.isPending ? "Creating..." : "Create Project"}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {!projects?.length ? (
        <div className="mt-8">
          <EmptyState
            icon={FolderKanban}
            message="No projects yet. Create your first project to get started."
            actionLabel="New Project"
            onAction={() => setOpen(true)}
          />
        </div>
      ) : (
        <div className="mt-6 grid gap-4">
          {projects.map((p) => (
            <Link key={p.id} to="/projects/$id" params={{ id: p.id }} className="block">
              <Card className="cursor-pointer transition-colors hover:bg-accent/50">
                <CardContent className="flex items-center justify-between p-4">
                  <div className="flex items-center gap-3">
                    <div className="flex h-9 w-9 items-center justify-center rounded-md bg-primary/10">
                      <FolderKanban className="h-4 w-4 text-primary" />
                    </div>
                    <div>
                      <p className="font-medium">{p.name}</p>
                      <p className="text-sm text-muted-foreground">
                        {p.description || "No description"}
                      </p>
                    </div>
                  </div>
                  <div className="flex items-center gap-3">
                    <Badge variant="secondary">{new Date(p.created_at).toLocaleDateString()}</Badge>
                    <ChevronRight className="h-4 w-4 text-muted-foreground" />
                  </div>
                </CardContent>
              </Card>
            </Link>
          ))}
        </div>
      )}
    </div>
  );
}
