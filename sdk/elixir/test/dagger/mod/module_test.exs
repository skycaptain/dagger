defmodule Dagger.Mod.ModuleTest do
  use ExUnit.Case, async: true

  alias Dagger.Mod.Module

  setup_all do
    dag = Dagger.connect!(connect_timeout: :timer.seconds(60))
    on_exit(fn -> Dagger.close(dag) end)

    %{dag: dag}
  end

  describe "define/1" do
    test "register module", %{dag: dag} do
      assert {:ok, _} = Module.define(dag, ObjectMod) |> Dagger.Module.id()
    end
  end
end
