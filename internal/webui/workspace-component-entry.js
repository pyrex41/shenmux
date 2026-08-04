// Browser-facing entry for the generated JCO component. Keeping the host
// setter beside the generated runtime makes the WIT filesystem import
// injectable in tests and in the OPFS/remote cache.
import { runtime } from "../../runtime/workspace-component/generated/workspace_component.js";
import { setWorkspaceFilesystem } from "./workspace-filesystem.js";

export { runtime, setWorkspaceFilesystem };
